package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/auth"
	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
)

type fakeStore struct {
	mu    sync.Mutex
	creds map[string]storage.Credential
	cli   map[string]storage.ClientKey
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		creds: map[string]storage.Credential{},
		cli:   map[string]storage.ClientKey{},
	}
}

func (f *fakeStore) ListCredentials() ([]storage.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]storage.Credential, 0, len(f.creds))
	for _, c := range f.creds {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) GetCredential(id string) (storage.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[id]
	if !ok {
		return storage.Credential{}, errNF("credential", id)
	}
	return c, nil
}

func (f *fakeStore) CreateCredential(in storage.CreateCredentialInput) (storage.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "cred_test1"
	now := time.Now().UTC().Truncate(time.Second)
	en := true
	if in.Enabled != nil {
		en = *in.Enabled
	}
	pr := 100
	if in.Priority != nil {
		pr = *in.Priority
	}
	c := storage.Credential{
		ID:           id,
		Name:         in.Name,
		Email:        in.Email,
		AccessToken:  in.AccessToken,
		RefreshToken: in.RefreshToken,
		ExpiresAt:    in.ExpiresAt,
		Enabled:      en,
		Priority:     pr,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	f.creds[id] = c
	return c, nil
}

func (f *fakeStore) UpdateCredential(c storage.Credential) (storage.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.creds[c.ID]; !ok {
		return storage.Credential{}, errNF("credential", c.ID)
	}
	f.creds[c.ID] = c
	return c, nil
}

func (f *fakeStore) DeleteCredential(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.creds[id]; !ok {
		return errNF("credential", id)
	}
	delete(f.creds, id)
	return nil
}

func (f *fakeStore) SetCredentialEnabled(id string, enabled bool) (storage.Credential, error) {
	c, err := f.GetCredential(id)
	if err != nil {
		return storage.Credential{}, err
	}
	c.Enabled = enabled
	return f.UpdateCredential(c)
}

func (f *fakeStore) SetCredentialPriority(id string, priority int) (storage.Credential, error) {
	c, err := f.GetCredential(id)
	if err != nil {
		return storage.Credential{}, err
	}
	c.Priority = priority
	return f.UpdateCredential(c)
}

func (f *fakeStore) ListClients() ([]storage.ClientKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]storage.ClientKey, 0, len(f.cli))
	for _, c := range f.cli {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) CreateClient(name string) (storage.CreateClientResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ck := storage.ClientKey{
		ID:        "cli_1",
		Name:      name,
		KeyHash:   "abc",
		Prefix:    "sk-test",
		CreatedAt: time.Now().UTC(),
	}
	f.cli[ck.ID] = ck
	return storage.CreateClientResult{Client: ck, Plaintext: "sk-test-plaintext-once"}, nil
}

func (f *fakeStore) DeleteClient(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cli[id]; !ok {
		return errNF("client", id)
	}
	delete(f.cli, id)
	return nil
}

func (f *fakeStore) GetBootstrapKeys() (apiKey, adminKey string, err error) {
	return "sk-api-fake", "sk-admin-fake", nil
}

func (f *fakeStore) SetAdminKey(newKey string) (adminKey string, generated bool, err error) {
	key := strings.TrimSpace(newKey)
	if key == "" {
		return "sk-admin-rotated", true, nil
	}
	return key, false, nil
}

func (f *fakeStore) RotateClientKey(id string) (storage.CreateClientResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cli[id]
	if !ok {
		return storage.CreateClientResult{}, errNF("client", id)
	}
	c.Prefix = "sk-rot"
	f.cli[id] = c
	return storage.CreateClientResult{Client: c, Plaintext: "sk-rotated-plaintext"}, nil
}


type nfErr struct{ kind, id string }

func (e nfErr) Error() string { return "storage: " + e.kind + " " + e.id + " not found" }

func errNF(kind, id string) error { return nfErr{kind, id} }

func TestAdminCredentialsMasked(t *testing.T) {
	store := newFakeStore()
	store.creds["cred_x"] = storage.Credential{
		ID:           "cred_x",
		Name:         "n",
		AccessToken:  "super-secret-access-token-value",
		RefreshToken: "super-secret-refresh-token-value",
		Enabled:      true,
		Priority:     10,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	h := &Handlers{
		Store:    store,
		AdminKey: "sk-admin-test",
		Config:   config.Default(),
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/credentials", nil)
	req.Header.Set("Authorization", "Bearer sk-admin-test")
	rr := httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "super-secret-access-token-value") {
		t.Fatalf("access token leaked: %s", body)
	}
	if strings.Contains(body, "super-secret-refresh-token-value") {
		t.Fatalf("refresh token leaked: %s", body)
	}
	if !strings.Contains(body, "***") {
		t.Fatalf("expected masked tokens, body=%s", body)
	}

	var parsed struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Credentials) != 1 {
		t.Fatalf("len=%d", len(parsed.Credentials))
	}
	at, _ := parsed.Credentials[0]["access_token"].(string)
	if at == "super-secret-access-token-value" || !strings.Contains(at, "***") {
		t.Fatalf("access_token not masked: %q", at)
	}
}

func TestAdminCredentialsPagination(t *testing.T) {
	store := newFakeStore()
	for i := 0; i < 45; i++ {
		id := fmt.Sprintf("cred_%02d", i)
		store.creds[id] = storage.Credential{ID: id, Enabled: true}
	}
	h := &Handlers{Store: store, AdminKey: "sk-admin-test", Config: config.Default()}
	req := httptest.NewRequest(http.MethodGet, "/admin/credentials?page=2&page_size=20", nil)
	req.Header.Set("Authorization", "Bearer sk-admin-test")
	rr := httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var parsed struct {
		Credentials []map[string]any `json:"credentials"`
		Page        int              `json:"page"`
		PageSize    int              `json:"page_size"`
		Total       int              `json:"total"`
		TotalPages  int              `json:"total_pages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Credentials) != 20 || parsed.Page != 2 || parsed.PageSize != 20 || parsed.Total != 45 || parsed.TotalPages != 3 {
		t.Fatalf("unexpected page: %+v credentials=%d", parsed, len(parsed.Credentials))
	}
}

func TestAdminRejectsBadKey(t *testing.T) {
	h := &Handlers{Store: newFakeStore(), AdminKey: "sk-admin-test", Config: config.Default()}
	req := httptest.NewRequest(http.MethodGet, "/admin/system", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestMaskSecret(t *testing.T) {
	if maskSecret("") != "" {
		t.Fatal("empty")
	}
	if maskSecret("short") != "***" {
		t.Fatal("short")
	}
	// Medium secrets fully redacted (no fingerprint).
	if maskSecret("abcdefghijklmnop") != "***" {
		t.Fatalf("medium should be fully redacted, got %q", maskSecret("abcdefghijklmnop"))
	}
	long := "abcdefghijklmnopqrstuvwxyz012345"
	m := maskSecret(long)
	if m == long || !strings.Contains(m, "***") || len(m) >= len(long) {
		t.Fatalf("mask=%q", m)
	}
}

func TestImportGrokIsIdempotent(t *testing.T) {
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := &Handlers{Store: store}
	body := `{"raw":{
		"https://auth.x.ai::client-test":{
			"key":"access-one",
			"refresh_token":"refresh-one",
			"user_id":"user-import",
			"email":"import@example.com",
			"oidc_client_id":"client-test"
		}
	}}`
	run := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/credentials/import-grok", strings.NewReader(body))
		rr := httptest.NewRecorder()
		h.ImportGrok(rr, req)
		return rr
	}
	first := run()
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := run()
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	creds, err := store.ListCredentials()
	if err != nil || len(creds) != 1 {
		t.Fatalf("credentials=%d err=%v", len(creds), err)
	}
	var response map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["updated"] != float64(1) || response["created"] != float64(0) {
		t.Fatalf("response=%v", response)
	}
}

type fakeDeviceOAuth struct {
	polls int
}

func (f *fakeDeviceOAuth) RequestDeviceCode(context.Context) (*auth.DeviceCodeResponse, error) {
	return &auth.DeviceCodeResponse{
		DeviceCode:              "device-secret",
		UserCode:                "ABCD-EFGH",
		VerificationURI:         "https://auth.x.ai/device",
		VerificationURIComplete: "https://auth.x.ai/device?user_code=ABCD-EFGH",
		ExpiresIn:               600,
		Interval:                1,
	}, nil
}

func (f *fakeDeviceOAuth) ExchangeDeviceCode(context.Context, string) (*auth.TokenSet, error) {
	f.polls++
	if f.polls == 1 {
		return nil, fmt.Errorf("authorization_pending")
	}
	return &auth.TokenSet{
		AccessToken:  "device-access",
		RefreshToken: "device-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}, nil
}

func TestDeviceCodeAdminFlow(t *testing.T) {
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oauth := &fakeDeviceOAuth{}
	h := &Handlers{
		Store:    store,
		OAuth:    oauth,
		AdminKey: "sk-admin-device-test",
		Config:   config.Default(),
	}
	handler := h.Handler()
	adminRequest := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-admin-device-test")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	start := adminRequest("/admin/oauth/device/start", `{}`)
	if start.Code != http.StatusCreated {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
	}
	if strings.Contains(start.Body.String(), "device-secret") {
		t.Fatalf("device_code leaked: %s", start.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil || started.SessionID == "" {
		t.Fatalf("start response=%s err=%v", start.Body.String(), err)
	}

	pollBody := `{"session_id":"` + started.SessionID + `"}`
	pending := adminRequest("/admin/oauth/device/poll", pollBody)
	if pending.Code != http.StatusAccepted {
		t.Fatalf("pending status=%d body=%s", pending.Code, pending.Body.String())
	}
	h.deviceMu.Lock()
	session := h.deviceSessions[started.SessionID]
	session.LastPollAt = time.Now().Add(-2 * session.Interval)
	h.deviceSessions[started.SessionID] = session
	h.deviceMu.Unlock()
	authorized := adminRequest("/admin/oauth/device/poll", pollBody)
	if authorized.Code != http.StatusCreated {
		t.Fatalf("authorized status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	creds, err := store.ListCredentials()
	if err != nil || len(creds) != 1 || creds[0].RefreshToken != "device-refresh" {
		t.Fatalf("credentials=%+v err=%v", creds, err)
	}
}

func TestSummarizePool(t *testing.T) {
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	cooldown := now.Add(time.Minute)
	success := now.Add(-time.Minute)
	summary := summarizePool([]storage.Credential{
		{ID: "available", Enabled: true, RefreshToken: "r", LastSuccessAt: &success},
		{ID: "cooling", Enabled: true, AccessToken: "a", CooldownUntil: &cooldown},
		{ID: "disabled", Enabled: false, AccessToken: "a"},
		{ID: "missing", Enabled: true},
		{ID: "expired", Enabled: true, AccessToken: "a", ExpiresAt: now.Add(-time.Minute)},
	}, now)
	if summary.Total != 5 || summary.Available != 1 || summary.Cooling != 1 ||
		summary.Disabled != 1 || summary.MissingTokens != 1 || summary.Expired != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.NextRecoveryAt == nil || summary.LastSuccessAt == nil {
		t.Fatalf("summary timestamps=%+v", summary)
	}
}

func TestDeviceCredentialInputUsesAccountIdentity(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{
		"sub":"user-device-1",
		"email":"device@example.com",
		"team_id":"team-device"
	}`))
	tokens := &auth.TokenSet{
		IDToken:      "header." + payload + ".signature",
		AccessToken:  "sensitive-access",
		RefreshToken: "sensitive-refresh",
	}
	input := deviceCredentialInput(tokens, "client-device")
	if input.UserID != "user-device-1" || input.Email != "device@example.com" ||
		input.TeamID != "team-device" || input.SourceKey != "device:user-device-1" {
		t.Fatalf("input=%+v", input)
	}
	if strings.Contains(input.SourceKey, "sensitive") {
		t.Fatalf("source key leaked token material: %s", input.SourceKey)
	}
	if !trustedVerificationURL("https://auth.x.ai/device") ||
		trustedVerificationURL("https://x.ai.example.invalid/device") ||
		trustedVerificationURL("javascript:alert(1)") {
		t.Fatal("verification URL trust boundary is incorrect")
	}
}
