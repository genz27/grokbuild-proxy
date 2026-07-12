package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GrokAuthEntry is one credential entry inside ~/.grok/auth.json.
// The CLI stores entries keyed by "https://auth.x.ai::<client_id>".
type GrokAuthEntry struct {
	// Key is the access JWT (CLI field name).
	Key           string `json:"key"`
	AuthMode      string `json:"auth_mode,omitempty"`
	CreateTime    string `json:"create_time,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	Email         string `json:"email,omitempty"`
	FirstName     string `json:"first_name,omitempty"`
	ProfileImage  string `json:"profile_image_asset_id,omitempty"`
	PrincipalType string `json:"principal_type,omitempty"`
	PrincipalID   string `json:"principal_id,omitempty"`
	TeamID        string `json:"team_id,omitempty"`
	CodingOptOut  bool   `json:"coding_data_retention_opt_out,omitempty"`
	RefreshToken  string `json:"refresh_token"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	OIDCIssuer    string `json:"oidc_issuer,omitempty"`
	OIDCClientID  string `json:"oidc_client_id,omitempty"`
}

// ImportedCredential is a normalized credential produced from auth.json.
type ImportedCredential struct {
	// SourceKey is the map key in auth.json (issuer::client_id).
	SourceKey    string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Email        string
	UserID       string
	TeamID       string
	OIDCIssuer   string
	OIDCClientID string
	AuthMode     string
	Raw          GrokAuthEntry
}

// DefaultGrokAuthPath returns ~/.grok/auth.json.
func DefaultGrokAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".grok", "auth.json")
	}
	return filepath.Join(home, ".grok", "auth.json")
}

// DefaultGrokAuthDir returns ~/.grok (import path jail root).
func DefaultGrokAuthDir() string {
	return filepath.Dir(DefaultGrokAuthPath())
}

// ResolveGrokAuthPath validates and resolves a path for reading Grok auth files.
// Empty path → DefaultGrokAuthPath(). Non-empty paths must resolve inside allowed roots
// (default: ~/.grok; optional extraRoots, e.g. proxy data_dir).
func ResolveGrokAuthPath(path string, extraRoots ...string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultGrokAuthPath()
	}
	// Reject null bytes before Abs.
	if strings.Contains(path, "\x00") {
		return "", fmt.Errorf("import grok auth: invalid path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("import grok auth: resolve path: %w", err)
	}
	// Resolve symlinks when the path exists so jail checks use the real target.
	// Empty/default paths go through the same checks (no symlink escape from ~/.grok).
	resolved := abs
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		resolved = real
	} else if !os.IsNotExist(err) {
		// Keep abs when target is missing (ReadFile will fail later with a clean error).
		// Other eval errors (permission) still use abs for allowlist check.
	}

	roots := make([]string, 0, 1+len(extraRoots))
	// Eval default root when possible so jail matches realpath of ~/.grok.
	defRoot := DefaultGrokAuthDir()
	if ar, err := filepath.Abs(defRoot); err == nil {
		defRoot = ar
		if real, err := filepath.EvalSymlinks(ar); err == nil {
			defRoot = real
		}
	}
	roots = append(roots, defRoot)
	for _, r := range extraRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		ar, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(ar); err == nil {
			ar = real
		}
		roots = append(roots, ar)
	}

	if !pathUnderAnyRoot(resolved, roots) {
		return "", fmt.Errorf("import grok auth: path not allowed (must be under ~/.grok or data_dir)")
	}
	return resolved, nil
}

func pathUnderAnyRoot(path string, roots []string) bool {
	clean := filepath.Clean(path)
	for _, root := range roots {
		root = filepath.Clean(root)
		if root == "" {
			continue
		}
		// Exact root match (directory itself) is not a file we want, but keep prefix rule.
		if clean == root {
			return true
		}
		prefix := root + string(os.PathSeparator)
		if strings.HasPrefix(clean, prefix) {
			return true
		}
	}
	return false
}

// ImportGrokAuthFile reads and parses a Grok CLI auth.json file.
// Empty path uses DefaultGrokAuthPath(). Paths outside ~/.grok (and optional extraRoots) are rejected.
func ImportGrokAuthFile(path string, extraRoots ...string) ([]ImportedCredential, error) {
	resolved, err := ResolveGrokAuthPath(path, extraRoots...)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		// Avoid echoing absolute path details beyond basename for missing/denied files.
		return nil, fmt.Errorf("import grok auth: read failed: %w", err)
	}
	return ParseGrokAuthJSON(data)
}

// ParseGrokAuthJSON parses Grok / xAI credential export documents.
//
// Accepted shapes:
//  1. Map keyed by "issuer::client_id" → entry (canonical CLI ~/.grok/auth.json)
//  2. Single entry object with key/refresh_token or access_token/refresh_token
//  3. {"accounts":[...]} / {"credentials":[...]} / {"tokens":[...]} arrays
//  4. Top-level JSON array of entries
//  5. CLIProxyAPI oauth export (access_token + refresh_token + email/expired)
//  6. Nested token object: {"token":{"access_token","refresh_token","expires_at"}}
//  7. accounts_output: oauth_access_token + oauth_refresh_token
func ParseGrokAuthJSON(data []byte) ([]ImportedCredential, error) {
	data = bytesTrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("import grok auth: empty document")
	}

	// Shape 4: top-level array.
	if data[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, fmt.Errorf("import grok auth: parse array: %w", err)
		}
		return parseEntryList(arr, "item")
	}

	// Shape 1: map of entries (canonical auth.json).
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &asMap); err == nil && looksLikeAuthMap(asMap) {
		out := make([]ImportedCredential, 0, len(asMap))
		for k, raw := range asMap {
			cred, ok, err := parseFlexibleEntry(k, raw)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			out = append(out, cred)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("import grok auth: no credential entries found")
		}
		return out, nil
	}

	// Shape 3: wrapper with arrays.
	var wrapper struct {
		Accounts    []json.RawMessage `json:"accounts"`
		Credentials []json.RawMessage `json:"credentials"`
		Tokens      []json.RawMessage `json:"tokens"`
		Items       []json.RawMessage `json:"items"`
		Data        []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		switch {
		case len(wrapper.Accounts) > 0:
			return parseEntryList(wrapper.Accounts, "accounts")
		case len(wrapper.Credentials) > 0:
			return parseEntryList(wrapper.Credentials, "credentials")
		case len(wrapper.Tokens) > 0:
			return parseEntryList(wrapper.Tokens, "tokens")
		case len(wrapper.Items) > 0:
			return parseEntryList(wrapper.Items, "items")
		case len(wrapper.Data) > 0:
			return parseEntryList(wrapper.Data, "data")
		}
	}

	// Shape 2 / 5 / 6 / 7: single flexible object.
	cred, ok, err := parseFlexibleEntry("default", data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("import grok auth: missing access/refresh tokens")
	}
	return []ImportedCredential{cred}, nil
}

func parseEntryList(items []json.RawMessage, label string) ([]ImportedCredential, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("import grok auth: empty %s list", label)
	}
	out := make([]ImportedCredential, 0, len(items))
	for i, raw := range items {
		cred, ok, err := parseFlexibleEntry(fmt.Sprintf("%s[%d]", label, i), raw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, cred)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("import grok auth: no credential entries found in %s", label)
	}
	return out, nil
}

// flexibleEntry covers CLI auth.json, CLIProxyAPI oauth export, and nested token blobs.
type flexibleEntry struct {
	Key               string          `json:"key"`
	AccessToken       string          `json:"access_token"`
	OAuthAccessToken  string          `json:"oauth_access_token"`
	RefreshToken      string          `json:"refresh_token"`
	OAuthRefreshToken string          `json:"oauth_refresh_token"`
	Email             string          `json:"email"`
	UserID            string          `json:"user_id"`
	Sub               string          `json:"sub"`
	PrincipalID       string          `json:"principal_id"`
	TeamID            string          `json:"team_id"`
	OIDCIssuer        string          `json:"oidc_issuer"`
	OIDCClientID      string          `json:"oidc_client_id"`
	ClientID          string          `json:"client_id"`
	AuthMode          string          `json:"auth_mode"`
	ExpiresAt         json.RawMessage `json:"expires_at"`
	Expired           json.RawMessage `json:"expired"`
	ExpiresIn         int64           `json:"expires_in"`
	Token             *flexibleToken  `json:"token"`
	UserInfo          *flexibleUser   `json:"userinfo"`
	IDTokenPayload    *flexibleUser   `json:"id_token_payload"`
}

type flexibleToken struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
	ExpiresIn    int64           `json:"expires_in"`
	TokenType    string          `json:"token_type"`
}

type flexibleUser struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func parseFlexibleEntry(sourceKey string, raw json.RawMessage) (ImportedCredential, bool, error) {
	var entry flexibleEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ImportedCredential{}, false, nil
	}

	access := firstNonEmpty(
		strings.TrimSpace(entry.Key),
		strings.TrimSpace(entry.AccessToken),
		strings.TrimSpace(entry.OAuthAccessToken),
	)
	refresh := firstNonEmpty(
		strings.TrimSpace(entry.RefreshToken),
		strings.TrimSpace(entry.OAuthRefreshToken),
	)
	if entry.Token != nil {
		if access == "" {
			access = strings.TrimSpace(entry.Token.AccessToken)
		}
		if refresh == "" {
			refresh = strings.TrimSpace(entry.Token.RefreshToken)
		}
	}
	if access == "" && refresh == "" {
		return ImportedCredential{}, false, nil
	}

	email := strings.TrimSpace(entry.Email)
	userID := firstNonEmpty(
		strings.TrimSpace(entry.UserID),
		strings.TrimSpace(entry.Sub),
		strings.TrimSpace(entry.PrincipalID),
	)
	if entry.UserInfo != nil {
		if email == "" {
			email = strings.TrimSpace(entry.UserInfo.Email)
		}
		if userID == "" {
			userID = strings.TrimSpace(entry.UserInfo.Sub)
		}
	}
	if entry.IDTokenPayload != nil {
		if email == "" {
			email = strings.TrimSpace(entry.IDTokenPayload.Email)
		}
		if userID == "" {
			userID = strings.TrimSpace(entry.IDTokenPayload.Sub)
		}
	}

	var exp time.Time
	var err error
	if exp, err = parseFlexibleTimeRaw(entry.ExpiresAt); err != nil {
		return ImportedCredential{}, false, fmt.Errorf("import grok auth: entry %q expires_at: %w", sourceKey, err)
	}
	if exp.IsZero() {
		if exp, err = parseFlexibleTimeRaw(entry.Expired); err != nil {
			return ImportedCredential{}, false, fmt.Errorf("import grok auth: entry %q expired: %w", sourceKey, err)
		}
	}
	if exp.IsZero() && entry.Token != nil {
		if exp, err = parseFlexibleTimeRaw(entry.Token.ExpiresAt); err != nil {
			return ImportedCredential{}, false, fmt.Errorf("import grok auth: entry %q token.expires_at: %w", sourceKey, err)
		}
		if exp.IsZero() && entry.Token.ExpiresIn > 0 {
			exp = time.Now().UTC().Add(time.Duration(entry.Token.ExpiresIn) * time.Second)
		}
	}
	if exp.IsZero() && entry.ExpiresIn > 0 {
		exp = time.Now().UTC().Add(time.Duration(entry.ExpiresIn) * time.Second)
	}

	clientID := firstNonEmpty(strings.TrimSpace(entry.OIDCClientID), strings.TrimSpace(entry.ClientID))
	issuer := strings.TrimSpace(entry.OIDCIssuer)
	if clientID == "" || issuer == "" {
		if iss, cid, ok := splitSourceKey(sourceKey); ok {
			if issuer == "" {
				issuer = iss
			}
			if clientID == "" {
				clientID = cid
			}
		}
	}
	if issuer == "" {
		issuer = Issuer
	}
	if clientID == "" {
		clientID = DefaultClientID
	}

	stableKey := sourceKey
	if sourceKey == "default" || strings.HasPrefix(sourceKey, "accounts[") ||
		strings.HasPrefix(sourceKey, "credentials[") || strings.HasPrefix(sourceKey, "tokens[") ||
		strings.HasPrefix(sourceKey, "items[") || strings.HasPrefix(sourceKey, "data[") ||
		strings.HasPrefix(sourceKey, "item[") {
		if userID != "" {
			stableKey = issuer + "::" + clientID + "::" + userID
		} else if email != "" {
			stableKey = issuer + "::" + clientID + "::" + email
		}
	}

	return ImportedCredential{
		SourceKey:    stableKey,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    exp,
		Email:        email,
		UserID:       userID,
		TeamID:       strings.TrimSpace(entry.TeamID),
		OIDCIssuer:   issuer,
		OIDCClientID: clientID,
		AuthMode:     strings.TrimSpace(entry.AuthMode),
		Raw: GrokAuthEntry{
			Key:          access,
			RefreshToken: refresh,
			Email:        email,
			UserID:       userID,
			TeamID:       strings.TrimSpace(entry.TeamID),
			OIDCIssuer:   issuer,
			OIDCClientID: clientID,
			AuthMode:     strings.TrimSpace(entry.AuthMode),
		},
	}, true, nil
}

// ToTokenSet converts an imported credential into a TokenSet.
func (c ImportedCredential) ToTokenSet() TokenSet {
	return TokenSet{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		TokenType:    "Bearer",
		ExpiresAt:    c.ExpiresAt,
	}
}


func isSingleCredentialObject(m map[string]json.RawMessage) bool {
	tokenFields := []string{"key", "access_token", "oauth_access_token", "refresh_token", "oauth_refresh_token"}
	for _, field := range tokenFields {
		raw, ok := m[field]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return true
		}
	}
	if raw, ok := m["token"]; ok {
		var tok struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if json.Unmarshal(raw, &tok) == nil && (strings.TrimSpace(tok.AccessToken) != "" || strings.TrimSpace(tok.RefreshToken) != "") {
			return true
		}
	}
	return false
}

func looksLikeAuthMap(m map[string]json.RawMessage) bool {
	if len(m) == 0 {
		return false
	}
	// Single-entry exports (CLIProxyAPI / nested token) are objects, not maps of accounts.
	if isSingleCredentialObject(m) {
		return false
	}
	wrapperOnly := true
	for k := range m {
		switch k {
		case "accounts", "credentials", "tokens", "items", "data":
			continue
		default:
			wrapperOnly = false
		}
	}
	if wrapperOnly {
		return false
	}
	for k, raw := range m {
		if strings.Contains(k, "::") {
			return true
		}
		var probe struct {
			Key               string `json:"key"`
			AccessToken       string `json:"access_token"`
			OAuthAccessToken  string `json:"oauth_access_token"`
			RefreshToken      string `json:"refresh_token"`
			OAuthRefreshToken string `json:"oauth_refresh_token"`
			Token             *struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			} `json:"token"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		if probe.Key != "" || probe.AccessToken != "" || probe.OAuthAccessToken != "" ||
			probe.RefreshToken != "" || probe.OAuthRefreshToken != "" {
			return true
		}
		if probe.Token != nil && (probe.Token.AccessToken != "" || probe.Token.RefreshToken != "") {
			return true
		}
	}
	return false
}

func splitSourceKey(key string) (issuer, clientID string, ok bool) {
	// Format: https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828[::user_id]
	parts := strings.Split(key, "::")
	if len(parts) < 2 {
		return "", "", false
	}
	issuer = strings.TrimSpace(parts[0])
	clientID = strings.TrimSpace(parts[1])
	if issuer == "" || clientID == "" {
		return "", "", false
	}
	return issuer, clientID, true
}

func parseFlexibleTimeRaw(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return time.Time{}, nil
		}
		return parseFlexibleTime(asString)
	}
	var asNumber json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&asNumber); err == nil {
		f, err := asNumber.Float64()
		if err != nil {
			return time.Time{}, err
		}
		if f <= 0 {
			return time.Time{}, nil
		}
		if f > 1e12 {
			return time.UnixMilli(int64(f)).UTC(), nil
		}
		return time.Unix(int64(f), 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unsupported time value %s", string(raw))
}

func parseFlexibleTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
	}
	var last error
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t.UTC(), nil
		}
		last = err
	}
	if n, err := json.Number(s).Int64(); err == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, last
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
