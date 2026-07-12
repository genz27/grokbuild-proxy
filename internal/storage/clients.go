package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// ClientKey is a local proxy API key record.
// Only the SHA-256 hash of the secret is stored; plaintext is returned once at creation.
type ClientKey struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	KeyHash   string         `json:"key_hash"`
	Prefix    string         `json:"prefix"`
	Disabled  bool           `json:"disabled"`
	CreatedAt time.Time      `json:"created_at"`
	Stats     map[string]any `json:"stats,omitempty"`
}

// clientsDoc is the on-disk envelope for clients.json.
type clientsDoc struct {
	Clients []ClientKey `json:"clients"`
}

// CreateClientResult is returned when a new client key is minted.
// Plaintext is the only opportunity to observe the raw secret.
type CreateClientResult struct {
	Client    ClientKey
	Plaintext string
}

// ListClients returns all client key records (no plaintext).
func (s *Store) ListClients() ([]ClientKey, error) {
	var out []ClientKey
	err := s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		out = append([]ClientKey(nil), doc.Clients...)
		return nil
	})
	return out, err
}

// GetClient returns a client key by id.
func (s *Store) GetClient(id string) (ClientKey, error) {
	var found ClientKey
	err := s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		for _, c := range doc.Clients {
			if c.ID == id {
				found = c
				return nil
			}
		}
		return fmt.Errorf("storage: client %q not found", id)
	})
	return found, err
}

// CreateClient mints a new sk-... key, stores only its sha256, returns plaintext once.
func (s *Store) CreateClient(name string) (CreateClientResult, error) {
	var result CreateClientResult
	err := s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		plain, err := generateSKKey()
		if err != nil {
			return err
		}
		id, err := newID("cli")
		if err != nil {
			return err
		}
		now := nowUTC()
		ck := ClientKey{
			ID:        id,
			Name:      name,
			KeyHash:   HashKey(plain),
			Prefix:    keyPrefix(plain),
			Disabled:  false,
			CreatedAt: now,
			Stats:     map[string]any{},
		}
		doc.Clients = append(doc.Clients, ck)
		if err := s.saveClients(doc); err != nil {
			return err
		}
		result = CreateClientResult{Client: ck, Plaintext: plain}
		return nil
	})
	return result, err
}

// DeleteClient removes a client key by id.
func (s *Store) DeleteClient(id string) error {
	return s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		next := make([]ClientKey, 0, len(doc.Clients))
		found := false
		for _, c := range doc.Clients {
			if c.ID == id {
				found = true
				continue
			}
			next = append(next, c)
		}
		if !found {
			return fmt.Errorf("storage: client %q not found", id)
		}
		doc.Clients = next
		return s.saveClients(doc)
	})
}

// SetClientDisabled toggles disabled flag.
func (s *Store) SetClientDisabled(id string, disabled bool) (ClientKey, error) {
	var updated ClientKey
	err := s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		for i := range doc.Clients {
			if doc.Clients[i].ID == id {
				doc.Clients[i].Disabled = disabled
				updated = doc.Clients[i]
				return s.saveClients(doc)
			}
		}
		return fmt.Errorf("storage: client %q not found", id)
	})
	return updated, err
}

// LookupClientByPlaintext finds a non-disabled client matching the raw key.
// Returns ok=false when not found or disabled.
// Hot path uses an in-process hash map without taking the disk flock.
func (s *Store) LookupClientByPlaintext(plaintext string) (ClientKey, bool, error) {
	if s == nil {
		return ClientKey{}, false, fmt.Errorf("storage: nil store")
	}
	if plaintext == "" {
		return ClientKey{}, false, nil
	}
	hash := HashKey(plaintext)

	s.mu.Lock()
	if s.clientCacheValid {
		c, hit := s.clientByHash[hash]
		s.mu.Unlock()
		if !hit || c.Disabled {
			return ClientKey{}, false, nil
		}
		return c, true, nil
	}
	s.mu.Unlock()

	var found ClientKey
	var ok bool
	err := s.withLock(func() error {
		if err := s.ensureClientCacheLocked(); err != nil {
			return err
		}
		c, hit := s.clientByHash[hash]
		if !hit || c.Disabled {
			return nil
		}
		found = c
		ok = true
		return nil
	})
	return found, ok, err
}

// ensureClientCacheLocked rebuilds the client hash map. Caller must hold s.mu
// (withLock already does).
func (s *Store) ensureClientCacheLocked() error {
	if s.clientCacheValid && s.clientByHash != nil {
		return nil
	}
	doc, err := s.loadClients()
	if err != nil {
		return err
	}
	s.rebuildClientCache(doc.Clients)
	return nil
}

func (s *Store) rebuildClientCache(clients []ClientKey) {
	m := make(map[string]ClientKey, len(clients))
	for _, c := range clients {
		if c.KeyHash == "" {
			continue
		}
		m[c.KeyHash] = c
	}
	s.clientByHash = m
	s.clientCacheValid = true
}

// bootstrapMeta holds process-local plaintext bootstrap secrets (mode 0600).
// Client keys are still stored hashed in clients.json; admin is never a client.
type bootstrapMeta struct {
	APIKey   string `json:"api_key,omitempty"`
	AdminKey string `json:"admin_key,omitempty"`
}

// EnsureBootstrapKeys ensures at least one client key and returns api/admin
// plaintext values suitable for first-run printing.
//
// Behavior:
//   - Configured (non-empty) api/admin keys always win.
//   - Otherwise keys are reused from data/meta.json when present.
//   - Otherwise a new sk-... key is generated, registered, and persisted to meta.json.
//
// generatedAPI / generatedAdmin are true only for the side that was newly
// minted this call. Callers must print each secret only when its flag is true
// (partial generation must not re-print the reused key).
//
// Admin key is not stored as a client key; only returned for process config.
// API key is always registered as a client key when generated or provided.
func (s *Store) EnsureBootstrapKeys(configuredAPIKey, configuredAdminKey string) (apiKey, adminKey string, generatedAPI, generatedAdmin bool, err error) {
	err = s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		meta, err := s.loadMeta()
		if err != nil {
			return err
		}

		apiKey = strings.TrimSpace(configuredAPIKey)
		adminKey = strings.TrimSpace(configuredAdminKey)
		previousAPIKey := strings.TrimSpace(meta.APIKey)
		metaDirty := false

		// Prefer configured → persisted meta → generate.
		if apiKey == "" {
			apiKey = strings.TrimSpace(meta.APIKey)
		}
		if adminKey == "" {
			adminKey = strings.TrimSpace(meta.AdminKey)
		}

		// Register a configured key only on first use or explicit rotation.
		// If the same key existed in meta but its client record was removed, that
		// deletion is a durable revocation and must survive restart.
		if apiKey != "" {
			h := HashKey(apiKey)
			exists := false
			for _, c := range doc.Clients {
				if c.KeyHash == h {
					exists = true
					break
				}
			}
			shouldRegister := previousAPIKey == "" || HashKey(previousAPIKey) != h
			if !exists && shouldRegister {
				id, err := newID("cli")
				if err != nil {
					return err
				}
				doc.Clients = append(doc.Clients, ClientKey{
					ID:        id,
					Name:      "bootstrap-api",
					KeyHash:   h,
					Prefix:    keyPrefix(apiKey),
					Disabled:  false,
					CreatedAt: nowUTC(),
					Stats:     map[string]any{},
				})
				if err := s.saveClients(doc); err != nil {
					return err
				}
			}
		} else {
			// Generate a fresh client key and return plaintext.
			plain, err := generateSKKey()
			if err != nil {
				return err
			}
			id, err := newID("cli")
			if err != nil {
				return err
			}
			ck := ClientKey{
				ID:        id,
				Name:      "bootstrap-api",
				KeyHash:   HashKey(plain),
				Prefix:    keyPrefix(plain),
				Disabled:  false,
				CreatedAt: nowUTC(),
				Stats:     map[string]any{},
			}
			doc.Clients = append(doc.Clients, ck)
			if err := s.saveClients(doc); err != nil {
				return err
			}
			apiKey = plain
			metaDirty = true
			generatedAPI = true
		}

		if adminKey == "" {
			adminKey, err = generateSKKey()
			if err != nil {
				return err
			}
			metaDirty = true
			generatedAdmin = true
		}

		// Persist bootstrap secrets so restarts reuse the same keys.
		// Always sync meta to the resolved keys when config was empty on either side,
		// or when we just generated.
		if metaDirty || strings.TrimSpace(meta.APIKey) != apiKey || strings.TrimSpace(meta.AdminKey) != adminKey {
			meta.APIKey = apiKey
			meta.AdminKey = adminKey
			if err := s.saveMeta(meta); err != nil {
				return err
			}
		}
		return nil
	})
	return apiKey, adminKey, generatedAPI, generatedAdmin, err
}

func (s *Store) loadMeta() (bootstrapMeta, error) {
	var meta bootstrapMeta
	err := readJSONFile(s.metaPath(), &meta)
	if err != nil {
		if os.IsNotExist(err) {
			return bootstrapMeta{}, nil
		}
		return bootstrapMeta{}, err
	}
	return meta, nil
}

func (s *Store) saveMeta(meta bootstrapMeta) error {
	return writeJSONFile(s.metaPath(), meta)
}

// HashKey returns hex-encoded SHA-256 of the raw client key.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func generateSKKey() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("storage: generate key: %w", err)
	}
	return "sk-" + hex.EncodeToString(b[:]), nil
}

func keyPrefix(plaintext string) string {
	// Short non-secret prefix for admin UI. Never store short secrets in full.
	const keep = 8
	if len(plaintext) <= keep {
		if strings.HasPrefix(plaintext, "sk-") {
			return "sk-***"
		}
		return "***"
	}
	return plaintext[:keep]
}

func (s *Store) loadClients() (clientsDoc, error) {
	var doc clientsDoc
	err := readJSONFile(s.clientsPath(), &doc)
	if err != nil {
		if os.IsNotExist(err) {
			return clientsDoc{Clients: []ClientKey{}}, nil
		}
		return clientsDoc{}, err
	}
	if doc.Clients == nil {
		doc.Clients = []ClientKey{}
	}
	return doc, nil
}

func (s *Store) saveClients(doc clientsDoc) error {
	if doc.Clients == nil {
		doc.Clients = []ClientKey{}
	}
	// Keep auth hot path map in sync with the durable write.
	s.rebuildClientCache(doc.Clients)
	return writeJSONFile(s.clientsPath(), doc)
}

// GetBootstrapKeys returns plaintext bootstrap secrets from meta.json.
// Empty strings mean the corresponding secret is not stored.
func (s *Store) GetBootstrapKeys() (apiKey, adminKey string, err error) {
	err = s.withLock(func() error {
		meta, err := s.loadMeta()
		if err != nil {
			return err
		}
		apiKey = strings.TrimSpace(meta.APIKey)
		adminKey = strings.TrimSpace(meta.AdminKey)
		return nil
	})
	return apiKey, adminKey, err
}

// SetAdminKey replaces the bootstrap admin key in meta.json.
// Empty newKey mints a fresh sk-... secret.
func (s *Store) SetAdminKey(newKey string) (adminKey string, generated bool, err error) {
	err = s.withLock(func() error {
		meta, err := s.loadMeta()
		if err != nil {
			return err
		}
		key := strings.TrimSpace(newKey)
		if key == "" {
			key, err = generateSKKey()
			if err != nil {
				return err
			}
			generated = true
		}
		if len(key) < 8 {
			return fmt.Errorf("storage: admin key too short")
		}
		api := strings.TrimSpace(meta.APIKey)
		if api != "" && api == key {
			return fmt.Errorf("storage: admin key must differ from api key")
		}
		meta.AdminKey = key
		if err := s.saveMeta(meta); err != nil {
			return err
		}
		adminKey = key
		return nil
	})
	return adminKey, generated, err
}

// RotateClientKey mints a new secret for an existing client id.
// Only the hash/prefix are persisted; plaintext is returned once.
func (s *Store) RotateClientKey(id string) (CreateClientResult, error) {
	var result CreateClientResult
	err := s.withLock(func() error {
		doc, err := s.loadClients()
		if err != nil {
			return err
		}
		for i := range doc.Clients {
			if doc.Clients[i].ID != id {
				continue
			}
			plain, err := generateSKKey()
			if err != nil {
				return err
			}
			doc.Clients[i].KeyHash = HashKey(plain)
			doc.Clients[i].Prefix = keyPrefix(plain)
			if err := s.saveClients(doc); err != nil {
				return err
			}
			// Keep bootstrap meta.api_key in sync when rotating the bootstrap client.
			meta, err := s.loadMeta()
			if err != nil {
				return err
			}
			if doc.Clients[i].Name == "bootstrap-api" {
				meta.APIKey = plain
				if err := s.saveMeta(meta); err != nil {
					return err
				}
			}
			result = CreateClientResult{Client: doc.Clients[i], Plaintext: plain}
			return nil
		}
		return fmt.Errorf("storage: client %q not found", id)
	})
	return result, err
}
