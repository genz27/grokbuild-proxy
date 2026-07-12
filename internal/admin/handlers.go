// Package admin implements the local admin HTTP API for credentials and clients.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/auth"
	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
	"github.com/GreyGunG/grokbuild-proxy/internal/upstream"
)

// Version is reported by GET /admin/system. Overridden by main via linker or SetVersion.
var Version = "dev"

// Store is the storage surface used by admin handlers.
type Store interface {
	ListCredentials() ([]storage.Credential, error)
	GetCredential(id string) (storage.Credential, error)
	CreateCredential(in storage.CreateCredentialInput) (storage.Credential, error)
	UpdateCredential(c storage.Credential) (storage.Credential, error)
	DeleteCredential(id string) error
	SetCredentialEnabled(id string, enabled bool) (storage.Credential, error)
	SetCredentialPriority(id string, priority int) (storage.Credential, error)
	ListClients() ([]storage.ClientKey, error)
	CreateClient(name string) (storage.CreateClientResult, error)
	DeleteClient(id string) error
	GetBootstrapKeys() (apiKey, adminKey string, err error)
	SetAdminKey(newKey string) (adminKey string, generated bool, err error)
	RotateClientKey(id string) (storage.CreateClientResult, error)
}

type credentialUpserter interface {
	UpsertCredential(in storage.CreateCredentialInput) (storage.Credential, bool, error)
}

type credentialBatchUpserter interface {
	UpsertCredentials(inputs []storage.CreateCredentialInput) ([]storage.UpsertCredentialResult, error)
}

// TokenService refreshes credentials and fetches billing.
type TokenService interface {
	ForceRefreshToken(ctx context.Context, credID string) (auth.TokenSet, storage.Credential, error)
	GetBillingSnapshot(ctx context.Context, credID string) (*upstream.BillingSnapshot, error)
}

// Handlers serves /admin/* endpoints.
type Handlers struct {
	Store  Store
	Tokens TokenService
	OAuth  DeviceOAuth
	Config config.Config
	// AdminKey is the plaintext admin bearer secret (process-local).
	AdminKey string
	// Version overrides package Version when non-empty.
	Version string
	// MaxBody limits JSON body size.
	MaxBody int64
	Metrics func() any

	deviceMu       sync.Mutex
	deviceSessions map[string]deviceSession
}

// maskedCredential is a credential view with secrets redacted.
type maskedCredential struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Email         string         `json:"email,omitempty"`
	UserID        string         `json:"user_id,omitempty"`
	TeamID        string         `json:"team_id,omitempty"`
	OIDCClientID  string         `json:"oidc_client_id,omitempty"`
	AccessToken   string         `json:"access_token"`  // masked
	RefreshToken  string         `json:"refresh_token"` // masked
	HasAccess     bool           `json:"has_access_token"`
	HasRefresh    bool           `json:"has_refresh_token"`
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

func maskCredential(c storage.Credential) maskedCredential {
	return maskedCredential{
		ID:            c.ID,
		Name:          c.Name,
		Email:         c.Email,
		UserID:        c.UserID,
		TeamID:        c.TeamID,
		OIDCClientID:  c.OIDCClientID,
		AccessToken:   maskSecret(c.AccessToken),
		RefreshToken:  maskSecret(c.RefreshToken),
		HasAccess:     strings.TrimSpace(c.AccessToken) != "",
		HasRefresh:    strings.TrimSpace(c.RefreshToken) != "",
		ExpiresAt:     c.ExpiresAt,
		Enabled:       c.Enabled,
		Priority:      c.Priority,
		FailureCount:  c.FailureCount,
		CooldownUntil: c.CooldownUntil,
		LastError:     c.LastError,
		LastUsedAt:    c.LastUsedAt,
		LastSuccessAt: c.LastSuccessAt,
		Billing:       c.Billing,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
}

// maskSecret never returns the full secret. Empty → empty; short → "***"; long → redacted.
// Only tokens longer than 24 chars expose a tiny fingerprint (2+2); never full short secrets.
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 24 {
		return "***"
	}
	return s[:2] + "***" + s[len(s)-2:]
}

func (h *Handlers) maxBody() int64 {
	if h != nil && h.MaxBody > 0 {
		return h.MaxBody
	}
	return 1 << 20
}

// importMaxBody allows larger bulk credential pastes while keeping other admin bodies smaller.
func (h *Handlers) importMaxBody() int64 {
	const floor int64 = 16 << 20 // 16 MiB
	if m := h.maxBody(); m > floor {
		return m
	}
	return floor
}

func (h *Handlers) version() string {
	if h != nil && h.Version != "" {
		return h.Version
	}
	return Version
}

// RequireAdmin is middleware that accepts only Authorization: Bearer <admin_key>.
func (h *Handlers) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h == nil || strings.TrimSpace(h.AdminKey) == "" {
			writeErr(w, http.StatusServiceUnavailable, "admin key not configured")
			return
		}
		got := bearerToken(r)
		if got == "" || !subtleConstantTimeEq(got, h.AdminKey) {
			writeErr(w, http.StatusUnauthorized, "invalid admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authz) >= 7 && strings.EqualFold(authz[:7], "Bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	// Also accept x-admin-key for convenience.
	if v := strings.TrimSpace(r.Header.Get("X-Admin-Key")); v != "" {
		return v
	}
	return ""
}

func subtleConstantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ListCredentials GET /admin/credentials
func (h *Handlers) ListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := h.Store.ListCredentials()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	page := positiveQueryInt(r, "page", 1)
	pageSize := positiveQueryInt(r, "page_size", 20)
	if pageSize > 100 {
		pageSize = 100
	}
	total := len(creds)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages > 0 && page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := min(start+pageSize, total)
	out := make([]maskedCredential, 0, end-start)
	for _, c := range creds[start:end] {
		out = append(out, maskCredential(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credentials": out,
		"pool":        summarizePool(creds, time.Now()),
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
	})
}

func positiveQueryInt(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

// CreateCredential POST /admin/credentials
func (h *Handlers) CreateCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string `json:"name"`
		Email        string `json:"email"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		Priority     *int   `json:"priority"`
		Enabled      *bool  `json:"enabled"`
		OIDCClientID string `json:"oidc_client_id"`
		UserID       string `json:"user_id"`
		TeamID       string `json:"team_id"`
	}
	if err := decodeJSON(r, h.maxBody(), &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var exp time.Time
	if strings.TrimSpace(body.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "expires_at must be RFC3339")
			return
		}
		exp = t
	}
	created, err := h.Store.CreateCredential(storage.CreateCredentialInput{
		Name:         body.Name,
		Email:        body.Email,
		UserID:       body.UserID,
		TeamID:       body.TeamID,
		OIDCClientID: body.OIDCClientID,
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresAt:    exp,
		Enabled:      body.Enabled,
		Priority:     body.Priority,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, maskCredential(created))
}

// ImportGrok POST /admin/credentials/import-grok
// Prefer body.raw JSON. path is optional and jailed to ~/.grok or data_dir.
func (h *Handlers) ImportGrok(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string          `json:"path"`
		Raw  json.RawMessage `json:"raw"`
	}
	// Body is optional; empty body → default path. Malformed JSON is 400 (not silent fallback).
	if err := decodeJSON(r, h.importMaxBody(), &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var imported []auth.ImportedCredential
	var err error
	if len(body.Raw) > 0 {
		raw := body.Raw
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			raw = json.RawMessage(asString)
		}
		imported, err = auth.ParseGrokAuthJSON(raw)
	} else {
		path := strings.TrimSpace(body.Path)
		var extraRoots []string
		if h != nil && strings.TrimSpace(h.Config.DataDir) != "" {
			extraRoots = append(extraRoots, h.Config.DataDir)
		}
		imported, err = auth.ImportGrokAuthFile(path, extraRoots...)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	credentials := make([]maskedCredential, 0, len(imported))
	results := make([]map[string]any, 0, len(imported))
	createdCount := 0
	updatedCount := 0
	failedCount := 0
	inputs := make([]storage.CreateCredentialInput, 0, len(imported))
	for _, ic := range imported {
		name := ic.Email
		if name == "" {
			name = ic.UserID
		}
		if name == "" {
			name = ic.SourceKey
		}
		inputs = append(inputs, storage.CreateCredentialInput{
			Name:         name,
			Email:        ic.Email,
			UserID:       ic.UserID,
			TeamID:       ic.TeamID,
			SourceKey:    ic.SourceKey,
			OIDCClientID: ic.OIDCClientID,
			AccessToken:  ic.AccessToken,
			RefreshToken: ic.RefreshToken,
			ExpiresAt:    ic.ExpiresAt,
		})
	}
	if batch, ok := h.Store.(credentialBatchUpserter); ok {
		batchResults, batchErr := batch.UpsertCredentials(inputs)
		if batchErr != nil {
			writeErr(w, http.StatusBadRequest, batchErr.Error())
			return
		}
		for i, item := range batchResults {
			status := "updated"
			if item.Created {
				status = "created"
				createdCount++
			} else {
				updatedCount++
			}
			results = append(results, map[string]any{"source_key": imported[i].SourceKey, "status": status, "id": item.Credential.ID})
			credentials = append(credentials, maskCredential(item.Credential))
		}
	} else {
		upserter, canUpsert := h.Store.(credentialUpserter)
		for i, input := range inputs {
			var c storage.Credential
			var wasCreated bool
			var cerr error
			if canUpsert {
				c, wasCreated, cerr = upserter.UpsertCredential(input)
			} else {
				c, cerr = h.Store.CreateCredential(input)
				wasCreated = cerr == nil
			}
			if cerr != nil {
				failedCount++
				results = append(results, map[string]any{
					"source_key": imported[i].SourceKey,
					"status":     "failed",
					"error":      cerr.Error(),
				})
				continue
			}
			if wasCreated {
				createdCount++
			} else {
				updatedCount++
			}
			status := "updated"
			if wasCreated {
				status = "created"
			}
			results = append(results, map[string]any{
				"source_key": imported[i].SourceKey,
				"status":     status,
				"id":         c.ID,
			})
			credentials = append(credentials, maskCredential(c))
		}
	}
	status := http.StatusOK
	if createdCount > 0 {
		status = http.StatusCreated
	}
	if failedCount > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{
		"imported":    len(credentials),
		"created":     createdCount,
		"updated":     updatedCount,
		"failed":      failedCount,
		"results":     results,
		"credentials": credentials,
	})
}

// DisableCredential POST /admin/credentials/{id}/disable
func (h *Handlers) DisableCredential(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled *bool `json:"enabled"`
		Disable *bool `json:"disable"`
	}
	_ = decodeJSON(r, h.maxBody(), &body)

	enabled := false
	if body.Enabled != nil {
		enabled = *body.Enabled
	} else if body.Disable != nil {
		enabled = !*body.Disable
	} else {
		// Toggle when no body fields.
		cur, err := h.Store.GetCredential(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		enabled = !cur.Enabled
	}
	updated, err := h.Store.SetCredentialEnabled(id, enabled)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, maskCredential(updated))
}

// SetPriority PUT /admin/credentials/{id}/priority
func (h *Handlers) SetPriority(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Priority int `json:"priority"`
	}
	if err := decodeJSON(r, h.maxBody(), &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.Store.SetCredentialPriority(id, body.Priority)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, maskCredential(updated))
}

// RefreshCredential POST /admin/credentials/{id}/refresh
func (h *Handlers) RefreshCredential(w http.ResponseWriter, r *http.Request, id string) {
	if h.Tokens == nil {
		writeErr(w, http.StatusServiceUnavailable, "token service not configured")
		return
	}
	_, cred, err := h.Tokens.ForceRefreshToken(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, maskCredential(cred))
}

// CredentialBilling GET /admin/credentials/{id}/billing
func (h *Handlers) CredentialBilling(w http.ResponseWriter, r *http.Request, id string) {
	if h.Tokens == nil {
		writeErr(w, http.StatusServiceUnavailable, "token service not configured")
		return
	}
	snap, err := h.Tokens.GetBillingSnapshot(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// DeleteCredential DELETE /admin/credentials/{id}
func (h *Handlers) DeleteCredential(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.Store.DeleteCredential(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ListClients GET /admin/clients
func (h *Handlers) ListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := h.Store.ListClients()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

// CreateClient POST /admin/clients
func (h *Handlers) CreateClient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = decodeJSON(r, h.maxBody(), &body)
	res, err := h.Store.CreateClient(body.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client":    res.Client,
		"plaintext": res.Plaintext,
		"api_key":   res.Plaintext,
	})
}

// DeleteClient DELETE /admin/clients/{id}
func (h *Handlers) DeleteClient(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.Store.DeleteClient(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// GetAdminKey GET /admin/secrets/admin-key
func (h *Handlers) GetAdminKey(w http.ResponseWriter, r *http.Request) {
	_, adminKey, err := h.Store.GetBootstrapKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Prefer live process admin key when available.
	live := strings.TrimSpace(h.AdminKey)
	if live != "" {
		adminKey = live
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"admin_key": adminKey,
		"prefix":    maskSecret(adminKey),
		"source":    "process",
	})
}

// SetAdminKey PUT /admin/secrets/admin-key
// Body: {"admin_key":"..."} or {"rotate":true}
func (h *Handlers) SetAdminKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AdminKey string `json:"admin_key"`
		Rotate   bool   `json:"rotate"`
	}
	if err := decodeJSON(r, h.maxBody(), &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	newKey := strings.TrimSpace(body.AdminKey)
	if body.Rotate {
		newKey = ""
	}
	if newKey == "" && !body.Rotate {
		writeErr(w, http.StatusBadRequest, "admin_key required (or set rotate=true)")
		return
	}
	adminKey, generated, err := h.Store.SetAdminKey(newKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Hot-reload process admin key so subsequent requests use the new secret.
	h.AdminKey = adminKey
	writeJSON(w, http.StatusOK, map[string]any{
		"admin_key": adminKey,
		"generated": generated,
		"prefix":    maskSecret(adminKey),
		"note":      "admin key updated in meta.json and current process; restart still loads the same key from meta",
	})
}

// RotateClient POST /admin/clients/{id}/rotate
func (h *Handlers) RotateClient(w http.ResponseWriter, r *http.Request, id string) {
	res, err := h.Store.RotateClientKey(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client":    res.Client,
		"plaintext": res.Plaintext,
		"api_key":   res.Plaintext,
		"note":      "plaintext shown once; store it now",
	})
}

// System GET /admin/system
func (h *Handlers) System(w http.ResponseWriter, r *http.Request) {
	credentials, err := h.Store.ListCredentials()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "credential store unavailable")
		return
	}
	out := map[string]any{
		"version": h.version(),
		"listen":  h.Config.Listen,
		"upstream": map[string]any{
			"base_url":          h.Config.Upstream.BaseURL,
			"client_version":    h.Config.Upstream.ClientVersion,
			"client_identifier": h.Config.Upstream.ClientIdentifier,
			"user_agent":        h.Config.Upstream.UserAgent,
			"token_auth":        h.Config.Upstream.TokenAuth,
		},
		"data_dir":     h.Config.DataDir,
		"chat_backend": h.Config.ChatBackend,
		"anthropic": map[string]any{
			"enabled": h.Config.Anthropic.Enabled,
		},
		"limits": h.Config.Limits,
		"pool":   summarizePool(credentials, time.Now()),
	}
	if h.Metrics != nil {
		if snap := h.Metrics(); snap != nil {
			out["metrics"] = snap
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeJSON(r *http.Request, max int64, dest any) error {
	if r == nil || r.Body == nil {
		return fmt.Errorf("missing body")
	}
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, max+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(raw)) > max {
		return fmt.Errorf("request body too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "admin_error",
			"code":    status,
		},
	})
}
