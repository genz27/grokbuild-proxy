package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Credential is a persisted Grok Build OAuth session used for upstream calls.
// Sensitive tokens are stored on disk with mode 0600; never log them.
type Credential struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Email         string         `json:"email,omitempty"`
	UserID        string         `json:"user_id,omitempty"`
	TeamID        string         `json:"team_id,omitempty"`
	SourceKey     string         `json:"source_key,omitempty"`
	OIDCClientID  string         `json:"oidc_client_id,omitempty"`
	AccessToken   string         `json:"access_token"`
	RefreshToken  string         `json:"refresh_token"`
	ExpiresAt     time.Time      `json:"expires_at"`
	Enabled       bool           `json:"enabled"`
	Priority      int            `json:"priority"`
	FailureCount  int            `json:"failure_count"`
	CooldownUntil *time.Time     `json:"cooldown_until,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
	LastUsedAt    *time.Time     `json:"last_used_at,omitempty"`
	LastSuccessAt *time.Time     `json:"last_success_at,omitempty"`
	Billing       map[string]any `json:"billing,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// credentialsDoc is the on-disk envelope for credentials.json.
type credentialsDoc struct {
	Credentials []Credential `json:"credentials"`
}

// ListCredentials returns all credentials sorted by priority desc, then id.
// Hot path clones an in-process cache without taking the disk flock.
func (s *Store) ListCredentials() ([]Credential, error) {
	if s == nil {
		return nil, fmt.Errorf("storage: nil store")
	}
	s.mu.Lock()
	if s.credCacheValid {
		out := append([]Credential(nil), s.credCache...)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	var out []Credential
	err := s.withLock(func() error {
		if s.credCacheValid {
			out = append([]Credential(nil), s.credCache...)
			return nil
		}
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		sorted := append([]Credential(nil), doc.Credentials...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Priority != sorted[j].Priority {
				return sorted[i].Priority > sorted[j].Priority
			}
			return sorted[i].ID < sorted[j].ID
		})
		s.credCache = sorted
		s.credCacheValid = true
		out = append([]Credential(nil), sorted...)
		return nil
	})
	return out, err
}

// GetCredential returns a credential by id.
func (s *Store) GetCredential(id string) (Credential, error) {
	if s == nil {
		return Credential{}, fmt.Errorf("storage: nil store")
	}
	s.mu.Lock()
	if s.credCacheValid {
		for _, c := range s.credCache {
			if c.ID == id {
				found := c
				s.mu.Unlock()
				return found, nil
			}
		}
		s.mu.Unlock()
		// Cache is authoritative when valid; missing means not found.
		return Credential{}, fmt.Errorf("storage: credential %q not found", id)
	}
	s.mu.Unlock()

	var found Credential
	err := s.withLock(func() error {
		if s.credCacheValid {
			for _, c := range s.credCache {
				if c.ID == id {
					found = c
					return nil
				}
			}
			return fmt.Errorf("storage: credential %q not found", id)
		}
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		for _, c := range doc.Credentials {
			if c.ID == id {
				found = c
				return nil
			}
		}
		return fmt.Errorf("storage: credential %q not found", id)
	})
	return found, err
}

// CreateCredentialInput is the mutable subset accepted on create.
type CreateCredentialInput struct {
	Name         string
	Email        string
	UserID       string
	TeamID       string
	SourceKey    string
	OIDCClientID string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Enabled      *bool
	Priority     *int
}

// CreateCredential appends a new credential and returns the stored record.
func (s *Store) CreateCredential(in CreateCredentialInput) (Credential, error) {
	if in.AccessToken == "" && in.RefreshToken == "" {
		return Credential{}, fmt.Errorf("storage: access_token or refresh_token required")
	}
	var created Credential
	err := s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		now := nowUTC()
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		priority := 100
		if in.Priority != nil {
			priority = *in.Priority
		}
		id, err := newID("cred")
		if err != nil {
			return err
		}
		created = Credential{
			ID:           id,
			Name:         in.Name,
			Email:        in.Email,
			UserID:       in.UserID,
			TeamID:       in.TeamID,
			SourceKey:    in.SourceKey,
			OIDCClientID: in.OIDCClientID,
			AccessToken:  in.AccessToken,
			RefreshToken: in.RefreshToken,
			ExpiresAt:    in.ExpiresAt.UTC().Truncate(time.Second),
			Enabled:      enabled,
			Priority:     priority,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		doc.Credentials = append(doc.Credentials, created)
		return s.saveCredentials(doc)
	})
	return created, err
}

// UpsertCredential imports a credential idempotently using stable account
// identity. Runtime health, enabled state, priority and creation time survive
// token rotation.
func (s *Store) UpsertCredential(in CreateCredentialInput) (Credential, bool, error) {
	if in.AccessToken == "" && in.RefreshToken == "" {
		return Credential{}, false, fmt.Errorf("storage: access_token or refresh_token required")
	}
	var result Credential
	created := false
	err := s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		now := nowUTC()
		for i := range doc.Credentials {
			if !sameCredentialIdentity(doc.Credentials[i], in) {
				continue
			}
			cur := doc.Credentials[i]
			if in.Name != "" {
				cur.Name = in.Name
			}
			if in.Email != "" {
				cur.Email = in.Email
			}
			if in.UserID != "" {
				cur.UserID = in.UserID
			}
			if in.TeamID != "" {
				cur.TeamID = in.TeamID
			}
			if in.SourceKey != "" {
				cur.SourceKey = in.SourceKey
			}
			if in.OIDCClientID != "" {
				cur.OIDCClientID = in.OIDCClientID
			}
			if in.AccessToken != "" {
				cur.AccessToken = in.AccessToken
			}
			if in.RefreshToken != "" {
				cur.RefreshToken = in.RefreshToken
			}
			if !in.ExpiresAt.IsZero() {
				cur.ExpiresAt = in.ExpiresAt.UTC().Truncate(time.Second)
			}
			cur.UpdatedAt = now
			doc.Credentials[i] = cur
			result = cur
			return s.saveCredentials(doc)
		}

		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		priority := 100
		if in.Priority != nil {
			priority = *in.Priority
		}
		id, err := newID("cred")
		if err != nil {
			return err
		}
		result = Credential{
			ID:           id,
			Name:         in.Name,
			Email:        in.Email,
			UserID:       in.UserID,
			TeamID:       in.TeamID,
			SourceKey:    in.SourceKey,
			OIDCClientID: in.OIDCClientID,
			AccessToken:  in.AccessToken,
			RefreshToken: in.RefreshToken,
			ExpiresAt:    in.ExpiresAt.UTC().Truncate(time.Second),
			Enabled:      enabled,
			Priority:     priority,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		doc.Credentials = append(doc.Credentials, result)
		created = true
		return s.saveCredentials(doc)
	})
	return result, created, err
}

// UpsertCredentialResult is one ordered result from UpsertCredentials.
type UpsertCredentialResult struct {
	Credential Credential
	Created    bool
}

// UpsertCredentials imports a batch under one lock and persists it once. This
// avoids rewriting the complete credential document for every imported account.
func (s *Store) UpsertCredentials(inputs []CreateCredentialInput) ([]UpsertCredentialResult, error) {
	results := make([]UpsertCredentialResult, 0, len(inputs))
	if len(inputs) == 0 {
		return results, nil
	}
	for _, in := range inputs {
		if strings.TrimSpace(in.AccessToken) == "" && strings.TrimSpace(in.RefreshToken) == "" {
			return nil, fmt.Errorf("storage: access_token or refresh_token required")
		}
	}
	err := s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		now := nowUTC()
		for _, in := range inputs {
			idx := -1
			for i := range doc.Credentials {
				if sameCredentialIdentity(doc.Credentials[i], in) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				cur := doc.Credentials[idx]
				mergeCredentialInput(&cur, in)
				cur.UpdatedAt = now
				doc.Credentials[idx] = cur
				results = append(results, UpsertCredentialResult{Credential: cur})
				continue
			}
			id, err := newID("cred")
			if err != nil {
				return err
			}
			enabled, priority := true, 100
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if in.Priority != nil {
				priority = *in.Priority
			}
			cur := Credential{ID: id, Enabled: enabled, Priority: priority, CreatedAt: now, UpdatedAt: now}
			mergeCredentialInput(&cur, in)
			doc.Credentials = append(doc.Credentials, cur)
			results = append(results, UpsertCredentialResult{Credential: cur, Created: true})
		}
		return s.saveCredentials(doc)
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func mergeCredentialInput(cur *Credential, in CreateCredentialInput) {
	if in.Name != "" {
		cur.Name = in.Name
	}
	if in.Email != "" {
		cur.Email = in.Email
	}
	if in.UserID != "" {
		cur.UserID = in.UserID
	}
	if in.TeamID != "" {
		cur.TeamID = in.TeamID
	}
	if in.SourceKey != "" {
		cur.SourceKey = in.SourceKey
	}
	if in.OIDCClientID != "" {
		cur.OIDCClientID = in.OIDCClientID
	}
	if in.AccessToken != "" {
		cur.AccessToken = in.AccessToken
	}
	if in.RefreshToken != "" {
		cur.RefreshToken = in.RefreshToken
	}
	if !in.ExpiresAt.IsZero() {
		cur.ExpiresAt = in.ExpiresAt.UTC().Truncate(time.Second)
	}
}

func sameCredentialIdentity(c Credential, in CreateCredentialInput) bool {
	if in.UserID != "" && c.UserID == in.UserID {
		return in.TeamID == "" || c.TeamID == "" || c.TeamID == in.TeamID
	}
	if in.Email != "" && c.Email != "" && strings.EqualFold(c.Email, in.Email) {
		return in.OIDCClientID == "" || c.OIDCClientID == "" || c.OIDCClientID == in.OIDCClientID
	}
	if in.SourceKey != "" && c.SourceKey == in.SourceKey {
		return true
	}
	return in.RefreshToken != "" && c.RefreshToken == in.RefreshToken
}

// UpdateCredential replaces an existing credential by id.
// The full Credential is expected (callers typically Get then mutate).
// Prefer PatchCredential for concurrent field updates to avoid lost-refresh races.
func (s *Store) UpdateCredential(c Credential) (Credential, error) {
	if c.ID == "" {
		return Credential{}, fmt.Errorf("storage: credential id required")
	}
	var updated Credential
	err := s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		idx := -1
		for i := range doc.Credentials {
			if doc.Credentials[i].ID == c.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("storage: credential %q not found", c.ID)
		}
		c.CreatedAt = doc.Credentials[idx].CreatedAt
		c.UpdatedAt = nowUTC()
		if !c.ExpiresAt.IsZero() {
			c.ExpiresAt = c.ExpiresAt.UTC().Truncate(time.Second)
		}
		doc.Credentials[idx] = c
		updated = c
		return s.saveCredentials(doc)
	})
	return updated, err
}

// PatchCredential loads a credential, applies mutate under the store lock, then saves.
// Use this for concurrent field updates (token rotate, last_used, enable, priority).
func (s *Store) PatchCredential(id string, mutate func(*Credential) error) (Credential, error) {
	if id == "" {
		return Credential{}, fmt.Errorf("storage: credential id required")
	}
	if mutate == nil {
		return Credential{}, fmt.Errorf("storage: mutate func required")
	}
	var updated Credential
	err := s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		idx := -1
		for i := range doc.Credentials {
			if doc.Credentials[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("storage: credential %q not found", id)
		}
		cur := doc.Credentials[idx]
		if err := mutate(&cur); err != nil {
			return err
		}
		cur.ID = id
		cur.CreatedAt = doc.Credentials[idx].CreatedAt
		cur.UpdatedAt = nowUTC()
		if !cur.ExpiresAt.IsZero() {
			cur.ExpiresAt = cur.ExpiresAt.UTC().Truncate(time.Second)
		}
		doc.Credentials[idx] = cur
		updated = cur
		return s.saveCredentials(doc)
	})
	return updated, err
}

// DeleteCredential removes a credential by id.
func (s *Store) DeleteCredential(id string) error {
	return s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		next := make([]Credential, 0, len(doc.Credentials))
		found := false
		for _, c := range doc.Credentials {
			if c.ID == id {
				found = true
				continue
			}
			next = append(next, c)
		}
		if !found {
			return fmt.Errorf("storage: credential %q not found", id)
		}
		doc.Credentials = next
		return s.saveCredentials(doc)
	})
}

// SetCredentialEnabled toggles the enabled flag atomically.
func (s *Store) SetCredentialEnabled(id string, enabled bool) (Credential, error) {
	return s.PatchCredential(id, func(c *Credential) error {
		c.Enabled = enabled
		return nil
	})
}

// SetCredentialPriority updates priority atomically.
func (s *Store) SetCredentialPriority(id string, priority int) (Credential, error) {
	return s.PatchCredential(id, func(c *Credential) error {
		c.Priority = priority
		return nil
	})
}

func (s *Store) loadCredentials() (credentialsDoc, error) {
	var doc credentialsDoc
	err := readJSONFile(s.credentialsPath(), &doc)
	if err != nil {
		if os.IsNotExist(err) {
			return credentialsDoc{Credentials: []Credential{}}, nil
		}
		return credentialsDoc{}, err
	}
	if doc.Credentials == nil {
		doc.Credentials = []Credential{}
	}
	return doc, nil
}

func (s *Store) saveCredentials(doc credentialsDoc) error {
	if doc.Credentials == nil {
		doc.Credentials = []Credential{}
	}
	// Rebuild sorted cache in place so the next ListCredentials is a pure clone.
	sorted := append([]Credential(nil), doc.Credentials...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority > sorted[j].Priority
		}
		return sorted[i].ID < sorted[j].ID
	})
	s.credCache = sorted
	s.credCacheValid = true
	return writeJSONFile(s.credentialsPath(), doc)
}

func newID(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("storage: generate id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}
