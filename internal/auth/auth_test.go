package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestParseGrokAuthJSON_MapShape(t *testing.T) {
	// Fixture mirrors ~/.grok/auth.json shape WITHOUT real tokens.
	const fixture = `{
  "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": {
    "key": "access-token-fixture",
    "auth_mode": "oidc",
    "create_time": "2026-07-09T13:32:31.815457884Z",
    "user_id": "user-fixture-id",
    "email": "fixture@example.com",
    "first_name": "fixture",
    "principal_type": "User",
    "principal_id": "user-fixture-id",
    "team_id": "team-fixture-id",
    "coding_data_retention_opt_out": false,
    "refresh_token": "refresh-token-fixture",
    "expires_at": "2026-07-09T19:32:31.815457884Z",
    "oidc_issuer": "https://auth.x.ai",
    "oidc_client_id": "b1a00492-073a-47ea-816f-4c329264a828"
  }
}`
	creds, err := ParseGrokAuthJSON([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("want 1 cred, got %d", len(creds))
	}
	c := creds[0]
	if c.AccessToken != "access-token-fixture" {
		t.Errorf("access = %q", c.AccessToken)
	}
	if c.RefreshToken != "refresh-token-fixture" {
		t.Errorf("refresh = %q", c.RefreshToken)
	}
	if c.Email != "fixture@example.com" {
		t.Errorf("email = %q", c.Email)
	}
	if c.UserID != "user-fixture-id" {
		t.Errorf("user_id = %q", c.UserID)
	}
	if c.TeamID != "team-fixture-id" {
		t.Errorf("team_id = %q", c.TeamID)
	}
	if c.OIDCClientID != DefaultClientID {
		t.Errorf("client_id = %q", c.OIDCClientID)
	}
	if c.OIDCIssuer != Issuer {
		t.Errorf("issuer = %q", c.OIDCIssuer)
	}
	if c.ExpiresAt.IsZero() {
		t.Fatal("expires_at not parsed")
	}
	if c.ExpiresAt.Year() != 2026 || c.ExpiresAt.Month() != 7 || c.ExpiresAt.Day() != 9 {
		t.Errorf("expires_at = %v", c.ExpiresAt)
	}
	ts := c.ToTokenSet()
	if ts.AccessToken != c.AccessToken || ts.RefreshToken != c.RefreshToken {
		t.Errorf("ToTokenSet mismatch: %+v", ts)
	}
}

func TestParseGrokAuthJSON_BareEntry(t *testing.T) {
	raw := `{"key":"a","refresh_token":"r","email":"e@x.ai","expires_at":"2026-01-02T03:04:05Z"}`
	creds, err := ParseGrokAuthJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].AccessToken != "a" || creds[0].RefreshToken != "r" {
		t.Fatalf("unexpected: %+v", creds)
	}
	if creds[0].OIDCClientID != DefaultClientID {
		t.Errorf("default client id missing: %q", creds[0].OIDCClientID)
	}
}

func TestParseGrokAuthJSON_SourceKeyFallback(t *testing.T) {
	raw := `{
  "https://auth.x.ai::custom-client-id": {
    "key": "k",
    "refresh_token": "r"
  }
}`
	creds, err := ParseGrokAuthJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if creds[0].OIDCClientID != "custom-client-id" {
		t.Errorf("client_id from key = %q", creds[0].OIDCClientID)
	}
	if creds[0].OIDCIssuer != "https://auth.x.ai" {
		t.Errorf("issuer from key = %q", creds[0].OIDCIssuer)
	}
}

func TestParseGrokAuthJSON_AccessTokenShape(t *testing.T) {
	raw := `{
  "access_token": "at-1",
  "refresh_token": "rt-1",
  "email": "clip@example.com",
  "sub": "user-1",
  "expired": "2026-07-11T08:35:42Z",
  "client_id": "b1a00492-073a-47ea-816f-4c329264a828"
}`
	creds, err := ParseGrokAuthJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("want 1, got %d", len(creds))
	}
	c := creds[0]
	if c.AccessToken != "at-1" || c.RefreshToken != "rt-1" {
		t.Fatalf("tokens: %+v", c)
	}
	if c.Email != "clip@example.com" || c.UserID != "user-1" {
		t.Fatalf("identity: %+v", c)
	}
	if c.ExpiresAt.IsZero() {
		t.Fatal("expires missing")
	}
}

func TestParseGrokAuthJSON_NestedTokenAndArray(t *testing.T) {
	raw := `{
  "token": {
    "access_token": "nested-at",
    "refresh_token": "nested-rt",
    "expires_at": 1783758942
  },
  "userinfo": {"email": "nested@example.com", "sub": "nested-user"}
}`
	creds, err := ParseGrokAuthJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].AccessToken != "nested-at" {
		t.Fatalf("%+v", creds)
	}
	if creds[0].Email != "nested@example.com" {
		t.Fatalf("email=%q", creds[0].Email)
	}

	arr := `[
  {"oauth_access_token":"a1","oauth_refresh_token":"r1","email":"a@x.ai"},
  {"oauth_access_token":"a2","oauth_refresh_token":"r2","email":"b@x.ai"}
]`
	creds, err = ParseGrokAuthJSON([]byte(arr))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("want 2, got %d", len(creds))
	}
	if creds[0].AccessToken != "a1" || creds[1].RefreshToken != "r2" {
		t.Fatalf("%+v", creds)
	}
}

func TestImportGrokAuthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	content := `{
  "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": {
    "key": "file-access",
    "refresh_token": "file-refresh",
    "email": "file@example.com",
    "expires_at": "2026-07-09T19:32:31Z"
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := ImportGrokAuthFile(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].AccessToken != "file-access" {
		t.Fatalf("unexpected: %+v", creds)
	}
}

func TestTokenSetExpired(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	ts := TokenSet{ExpiresAt: now.Add(2 * time.Minute)}
	if ts.Expired(now, 3*time.Minute) {
		// 2m left, skew 3m → expired
	} else {
		t.Fatal("expected expired under skew")
	}
	if ts.Expired(now, 30*time.Second) {
		t.Fatal("should still be valid with small skew")
	}
	if (TokenSet{}).Expired(now, time.Minute) {
		t.Fatal("zero ExpiresAt should not be expired")
	}
}

func TestOAuthRefresh(t *testing.T) {
	var gotGrant, gotClient, gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/x-www-form-urlencoded") {
			t.Errorf("content-type %q", ct)
		}
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotClient = r.Form.Get("client_id")
		gotRefresh = r.Form.Get("refresh_token")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)

	c := &OAuthClient{
		HTTPClient:    srv.Client(),
		TokenEndpoint: srv.URL,
		ClientID:      DefaultClientID,
	}
	ts, err := c.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if gotGrant != "refresh_token" {
		t.Errorf("grant_type=%q", gotGrant)
	}
	if gotClient != DefaultClientID {
		t.Errorf("client_id=%q", gotClient)
	}
	if gotRefresh != "old-refresh" {
		t.Errorf("refresh_token=%q", gotRefresh)
	}
	if ts.AccessToken != "new-access" || ts.RefreshToken != "new-refresh" {
		t.Errorf("token set: %+v", ts)
	}
	if ts.ExpiresIn != 3600 || ts.ExpiresAt.IsZero() {
		t.Errorf("expiry: %+v", ts)
	}
}

func TestOAuthRefresh_PreservesRefreshWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a2",
			"expires_in":   60,
		})
	}))
	t.Cleanup(srv.Close)
	c := &OAuthClient{HTTPClient: srv.Client(), TokenEndpoint: srv.URL}
	ts, err := c.Refresh(context.Background(), "keep-me")
	if err != nil {
		t.Fatal(err)
	}
	if ts.RefreshToken != "keep-me" {
		t.Errorf("refresh not preserved: %q", ts.RefreshToken)
	}
}

func TestOAuthDiscover_RejectsNonXAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": "https://evil.example/authorize",
			"token_endpoint":         "https://evil.example/token",
		})
	}))
	t.Cleanup(srv.Close)
	c := &OAuthClient{HTTPClient: srv.Client(), DiscoveryURL: srv.URL}
	if _, err := c.Discover(context.Background()); err == nil {
		t.Fatal("expected reject non-x.ai host")
	}
}

func TestRefresherSingleflight(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // force overlap
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "shared-access",
			"refresh_token": "rotated-refresh",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)

	oauth := &OAuthClient{HTTPClient: srv.Client(), TokenEndpoint: srv.URL}
	ref := &Refresher{
		OAuth: oauth,
		Skew:  time.Minute,
		Now:   func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
	}

	current := TokenSet{
		AccessToken:  "old",
		RefreshToken: "r1",
		ExpiresAt:    time.Date(2026, 7, 9, 12, 0, 10, 0, time.UTC), // within skew → refresh
	}

	var persistCount atomic.Int32
	persist := func(ctx context.Context, next TokenSet) error {
		persistCount.Add(1)
		if next.AccessToken != "shared-access" {
			return fmt.Errorf("bad token %q", next.AccessToken)
		}
		return nil
	}

	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	resCh := make(chan TokenSet, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ts, err := ref.EnsureAccess(context.Background(), "cred-1", current, persist)
			if err != nil {
				errCh <- err
				return
			}
			resCh <- ts
		}()
	}
	wg.Wait()
	close(errCh)
	close(resCh)
	for err := range errCh {
		t.Fatalf("ensure: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 refresh call, got %d", calls.Load())
	}
	if persistCount.Load() != 1 {
		t.Fatalf("expected 1 persist, got %d", persistCount.Load())
	}
	for ts := range resCh {
		if ts.AccessToken != "shared-access" || ts.RefreshToken != "rotated-refresh" {
			t.Fatalf("unexpected token set: %+v", ts)
		}
	}
}

func TestRefresherEnsureAccess_NoRefreshWhenValid(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	ref := &Refresher{
		OAuth: &OAuthClient{HTTPClient: srv.Client(), TokenEndpoint: srv.URL},
		Skew:  time.Minute,
		Now:   func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
	}
	current := TokenSet{
		AccessToken:  "still-good",
		RefreshToken: "r",
		ExpiresAt:    time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
	}
	ts, err := ref.EnsureAccess(context.Background(), "c", current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "still-good" {
		t.Errorf("got %q", ts.AccessToken)
	}
	if calls.Load() != 0 {
		t.Fatalf("should not refresh, calls=%d", calls.Load())
	}
}

func TestForceRefreshAlwaysHitsNetwork(t *testing.T) {
	// Even when cache has a still-valid access token, ForceRefresh must network-refresh
	// (401 path / admin force). Returning the cached AT would retry the failed token.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "fresh-refresh",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	ref := &Refresher{
		OAuth: &OAuthClient{HTTPClient: srv.Client(), TokenEndpoint: srv.URL},
		Skew:  time.Minute,
		Now:   func() time.Time { return now },
	}
	// Seed cache with a still-valid access token.
	ref.store("cred-force", TokenSet{
		AccessToken:  "cached-still-valid",
		RefreshToken: "rt-cached",
		ExpiresAt:    now.Add(2 * time.Hour),
		TokenType:    "Bearer",
	})
	ts, err := ref.ForceRefresh(context.Background(), "cred-force", TokenSet{
		AccessToken:  "old-at",
		RefreshToken: "rt-caller",
		ExpiresAt:    now.Add(-time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("ForceRefresh must hit network even with valid cache, calls=%d", calls.Load())
	}
	if ts.AccessToken != "fresh-access" {
		t.Fatalf("expected fresh access, got %q", ts.AccessToken)
	}
	if ts.RefreshToken != "fresh-refresh" {
		t.Fatalf("expected fresh refresh, got %q", ts.RefreshToken)
	}
}

func TestRefresherWaiterCancellationDoesNotHang(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ref := &Refresher{
		OAuth:   &OAuthClient{HTTPClient: client, TokenEndpoint: "https://auth.x.ai/test-token"},
		Timeout: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := ref.ForceRefresh(ctx, "cancelled", TokenSet{RefreshToken: "rt"}, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context canceled", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("cancelled waiter returned too slowly")
	}
}

func TestRefresherSharedOperationHasDeadline(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ref := &Refresher{
		OAuth:   &OAuthClient{HTTPClient: client, TokenEndpoint: "https://auth.x.ai/test-token"},
		Timeout: 25 * time.Millisecond,
	}
	start := time.Now()
	_, err := ref.ForceRefresh(context.Background(), "timeout", TokenSet{RefreshToken: "rt"}, nil)
	if err == nil {
		t.Fatal("expected refresh timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("refresh timeout was not enforced")
	}
}

func TestRequestDeviceCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != DefaultClientID {
			t.Errorf("client_id=%q", r.Form.Get("client_id"))
		}
		if r.Form.Get("scope") == "" {
			t.Error("missing scope")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://auth.x.ai/device",
			"verification_uri_complete": "https://auth.x.ai/device?user_code=ABCD-EFGH",
			"expires_in":                1800,
			"interval":                  5,
		})
	}))
	t.Cleanup(srv.Close)
	c := &OAuthClient{HTTPClient: srv.Client(), DeviceAuthEndpoint: srv.URL}
	dc, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABCD-EFGH" || dc.DeviceCode != "dev" {
		t.Fatalf("%+v", dc)
	}
}

func TestRequestDeviceCodeRejectsUntrustedVerificationURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.invalid/device",
			"expires_in":       1800,
			"interval":         5,
		})
	}))
	t.Cleanup(srv.Close)
	client := &OAuthClient{HTTPClient: srv.Client(), DeviceAuthEndpoint: srv.URL}
	if _, err := client.RequestDeviceCode(context.Background()); err == nil {
		t.Fatal("expected untrusted verification URI to be rejected")
	}
}

func TestConstants(t *testing.T) {
	if DefaultClientID != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Fatal(DefaultClientID)
	}
	if !strings.Contains(DefaultScope, "grok-cli:access") {
		t.Fatal(DefaultScope)
	}
	if Issuer != "https://auth.x.ai" {
		t.Fatal(Issuer)
	}
}

func TestResolveGrokAuthPathJail(t *testing.T) {
	// Outside home .grok must be rejected.
	if _, err := ResolveGrokAuthPath("/etc/passwd"); err == nil {
		t.Fatal("expected /etc/passwd rejected")
	}
	// Empty uses default (may or may not exist).
	p, err := ResolveGrokAuthPath("")
	if err != nil {
		t.Fatal(err)
	}
	if p == "" {
		t.Fatal("empty path should resolve to default")
	}
	// data_dir root allowed when provided.
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"key":"a","refresh_token":"b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveGrokAuthPath(authPath, dir)
	if err != nil {
		t.Fatalf("data_dir path should be allowed: %v", err)
	}
	if got == "" {
		t.Fatal("empty resolved")
	}
	// traversal out of root rejected
	if _, err := ResolveGrokAuthPath(filepath.Join(dir, "..", "outside.json"), dir); err == nil {
		// may resolve outside — must reject
		t.Fatal("expected path escape rejected")
	}
}
