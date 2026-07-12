// Package config loads and validates grokbuild-proxy configuration.
package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root runtime configuration for grokbuild-proxy.
type Config struct {
	Listen            string          `yaml:"listen"`
	DataDir           string          `yaml:"data_dir"`
	APIKey            string          `yaml:"api_key"`
	AdminKey          string          `yaml:"admin_key"`
	AllowPublicListen bool            `yaml:"allow_public_listen"`
	Upstream          UpstreamConfig  `yaml:"upstream"`
	OAuth             OAuthConfig     `yaml:"oauth"`
	ChatBackend       string          `yaml:"chat_backend"`
	Anthropic         AnthropicConfig `yaml:"anthropic"`
	LB                LBConfig        `yaml:"lb"`
	Limits            LimitsConfig    `yaml:"limits"`
	Logging           LoggingConfig   `yaml:"logging"`
}

// UpstreamConfig controls how requests are sent to cli-chat-proxy.grok.com.
type UpstreamConfig struct {
	BaseURL          string `yaml:"base_url"`
	ClientVersion    string `yaml:"client_version"`
	ClientIdentifier string `yaml:"client_identifier"`
	UserAgent        string `yaml:"user_agent"`
	TokenAuth        string `yaml:"token_auth"`
}

// OAuthConfig holds OIDC / device-flow settings for xAI auth.
type OAuthConfig struct {
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"client_id"`
	Scope        string `yaml:"scope"`
	CallbackAddr string `yaml:"callback_addr"`
}

// AnthropicConfig controls Claude Code / Anthropic Messages entry behavior.
type AnthropicConfig struct {
	Enabled             bool              `yaml:"enabled"`
	ModelAliases        map[string]string `yaml:"model_aliases"`
	PassthroughPrefixes []string          `yaml:"passthrough_prefixes"`
	StripUnknownBetas   bool              `yaml:"strip_unknown_betas"`
	CountTokens         bool              `yaml:"count_tokens"`
	// Context protection / auto-compact (Claude Code long-session support).
	// SoftInputTokens triggers auto-compact; MaxInputTokens hard-rejects after compact.
	// Defaults applied when zero: soft=420000, max=480000, tool_result=120000, keep=16.
	AutoCompact        *bool `yaml:"auto_compact"`
	SoftInputTokens    int   `yaml:"soft_input_tokens"`
	MaxInputTokens     int   `yaml:"max_input_tokens"`
	MaxToolResultChars int   `yaml:"max_tool_result_chars"`
	KeepRecentMessages int   `yaml:"keep_recent_messages"`
	PreserveCacheHints *bool `yaml:"preserve_cache_hints"`
}

// LBConfig controls multi-credential selection and sticky sessions.
type LBConfig struct {
	Strategy       string         `yaml:"strategy"`
	StickyTTLSec   int            `yaml:"sticky_ttl_sec"`
	RefreshSkewSec int            `yaml:"refresh_skew_sec"`
	// MaxAttempts caps credential failover per request (0 = package default).
	MaxAttempts int `yaml:"max_attempts"`
	Cooldown    CooldownConfig `yaml:"cooldown"`
	Prefetch    PrefetchConfig `yaml:"prefetch"`
	// SoftDemoteOn429 temporarily lowers pick preference after 429s (in-memory).
	// Success clears the demotion. Zero/false keeps default enabled behavior via Defaults.
	SoftDemoteOn429 *bool `yaml:"soft_demote_on_429"`
}

// PrefetchConfig controls background access-token pre-refresh.
type PrefetchConfig struct {
	Enabled     *bool `yaml:"enabled"`
	IntervalSec int   `yaml:"interval_sec"`
	MaxPerTick  int   `yaml:"max_per_tick"`
	Concurrency int   `yaml:"concurrency"`
}

// CooldownConfig is exponential backoff bounds for failed credentials.
type CooldownConfig struct {
	BaseSec int `yaml:"base_sec"`
	MaxSec  int `yaml:"max_sec"`
	// FreeUsageExhaustedSec cools accounts after free-usage-exhausted (rolling 24h quota).
	// 0 uses the default (20h). This is intentionally longer than MaxSec.
	FreeUsageExhaustedSec int `yaml:"free_usage_exhausted_sec"`
}

// LimitsConfig enforces request size, timeout and concurrency caps.
type LimitsConfig struct {
	MaxBodyBytes      int64 `yaml:"max_body_bytes"`
	RequestTimeoutSec int   `yaml:"request_timeout_sec"`
	MaxConcurrent     int   `yaml:"max_concurrent"`
}

// LoggingConfig controls structured logging verbosity.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Default returns a Config aligned with plan.md defaults.
func Default() Config {
	return Config{
		Listen:            "127.0.0.1:8080",
		DataDir:           "./data",
		APIKey:            "",
		AdminKey:          "",
		AllowPublicListen: false,
		Upstream: UpstreamConfig{
			BaseURL:          "https://cli-chat-proxy.grok.com/v1",
			ClientVersion:    "0.2.93",
			ClientIdentifier: "grok-pager",
			UserAgent:        "grok-pager/0.2.93 grok-shell/0.2.93 (linux; x86_64)",
			TokenAuth:        "xai-grok-cli",
		},
		OAuth: OAuthConfig{
			Issuer:       "https://auth.x.ai",
			ClientID:     "b1a00492-073a-47ea-816f-4c329264a828",
			Scope:        "openid profile email offline_access grok-cli:access api:access",
			CallbackAddr: "127.0.0.1:56122",
		},
		ChatBackend: "responses",
		Anthropic: AnthropicConfig{
			Enabled: true,
			ModelAliases: map[string]string{
				"claude-sonnet-4":   "grok-4.5",
				"claude-sonnet-4-0": "grok-4.5",
				"claude-sonnet-4-6": "grok-4.5",
				"claude-sonnet-5":   "grok-4.5",
				"claude-opus-4":     "grok-4.5",
				"claude-opus-4-6":   "grok-4.5",
				"claude-opus-4-7":   "grok-4.5",
				"claude-opus-4-8":   "grok-4.5",
				"claude-haiku-4":    "grok-composer-2.5-fast",
				"claude-haiku-4-5":  "grok-composer-2.5-fast",
				"sonnet":            "grok-4.5",
				"opus":              "grok-4.5",
				"haiku":             "grok-composer-2.5-fast",
			},
			PassthroughPrefixes: []string{"grok-"},
			StripUnknownBetas:   true,
			CountTokens:         true,
			SoftInputTokens:     400000,
			MaxInputTokens:      460000,
			MaxToolResultChars:  120000,
			KeepRecentMessages:  16,
		},
		LB: LBConfig{
			Strategy:       "priority_rr",
			StickyTTLSec:   3600,
			RefreshSkewSec: 180,
			// High enough to skip free-exhausted accounts without surfacing 429 to clients.
			MaxAttempts: 32,
			Cooldown: CooldownConfig{
				BaseSec:               300,
				MaxSec:                3600,
				FreeUsageExhaustedSec: 72000, // 20h — free quota is a rolling 24h window
			},
			Prefetch: PrefetchConfig{
				IntervalSec: 30,
				MaxPerTick:  128,
				Concurrency: 16,
			},
		},
		Limits: LimitsConfig{
			MaxBodyBytes:      20 * 1024 * 1024,
			RequestTimeoutSec: 600,
			// Sized for multi-thousand concurrent SSE sessions on a single node.
			// Raise further only after host FD/CPU headroom is confirmed.
			MaxConcurrent: 2048,
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads a YAML file and merges it over Default().
// Missing file returns Default() with no error when path is empty.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		applyListenEnvironment(&cfg)
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("config file not found: %s: %w", path, err)
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return cfg, fmt.Errorf("parse config %s: multiple YAML documents are not supported", path)
		}
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Listen overrides must be applied before Validate. This lets an operator
	// safely narrow a config-file public bind to loopback at runtime.
	applyListenEnvironment(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyListenEnvironment(cfg *Config) {
	if cfg == nil {
		return
	}
	if value := strings.TrimSpace(os.Getenv("LISTEN")); value != "" {
		cfg.Listen = value
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALLOW_PUBLIC_LISTEN"))) {
	case "1", "true", "yes", "on":
		cfg.AllowPublicListen = true
	}
}

// Validate checks required fields and numeric ranges.
func (c Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if c.Upstream.BaseURL == "" {
		return fmt.Errorf("upstream.base_url must not be empty")
	}
	if u, err := url.Parse(c.Upstream.BaseURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("upstream.base_url must be an absolute https URL")
	}
	if c.ChatBackend != "responses" {
		return fmt.Errorf("chat_backend must be responses, got %q", c.ChatBackend)
	}
	issuer, err := url.Parse(c.OAuth.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" {
		return fmt.Errorf("oauth.issuer must be an absolute https URL")
	}
	issuerHost := strings.ToLower(issuer.Hostname())
	if issuerHost != "x.ai" && !strings.HasSuffix(issuerHost, ".x.ai") {
		return fmt.Errorf("oauth.issuer host must be x.ai")
	}
	if c.LB.Strategy != "priority_rr" && c.LB.Strategy != "round_robin" {
		return fmt.Errorf("lb.strategy must be priority_rr or round_robin, got %q", c.LB.Strategy)
	}
	if c.LB.StickyTTLSec < 0 {
		return fmt.Errorf("lb.sticky_ttl_sec must be >= 0")
	}
	if c.LB.RefreshSkewSec < 0 {
		return fmt.Errorf("lb.refresh_skew_sec must be >= 0")
	}
	if c.LB.Cooldown.BaseSec < 0 || c.LB.Cooldown.MaxSec < 0 {
		return fmt.Errorf("lb.cooldown base_sec/max_sec must be >= 0")
	}
	if c.LB.Cooldown.MaxSec > 0 && c.LB.Cooldown.BaseSec > c.LB.Cooldown.MaxSec {
		return fmt.Errorf("lb.cooldown.base_sec must be <= max_sec")
	}
	if c.LB.Cooldown.FreeUsageExhaustedSec < 0 {
		return fmt.Errorf("lb.cooldown.free_usage_exhausted_sec must be >= 0")
	}
	if c.LB.MaxAttempts < 0 {
		return fmt.Errorf("lb.max_attempts must be >= 0")
	}
	if c.LB.Prefetch.IntervalSec < 0 {
		return fmt.Errorf("lb.prefetch.interval_sec must be >= 0")
	}
	if c.LB.Prefetch.MaxPerTick < 0 {
		return fmt.Errorf("lb.prefetch.max_per_tick must be >= 0")
	}
	if c.LB.Prefetch.Concurrency < 0 {
		return fmt.Errorf("lb.prefetch.concurrency must be >= 0")
	}
	if c.Limits.MaxBodyBytes <= 0 {
		return fmt.Errorf("limits.max_body_bytes must be > 0")
	}
	if c.Limits.RequestTimeoutSec <= 0 {
		return fmt.Errorf("limits.request_timeout_sec must be > 0")
	}
	if c.Limits.MaxConcurrent <= 0 {
		return fmt.Errorf("limits.max_concurrent must be > 0")
	}
	if c.Anthropic.SoftInputTokens < 0 || c.Anthropic.MaxInputTokens < 0 {
		return fmt.Errorf("anthropic soft/max input tokens must be >= 0")
	}
	if c.Anthropic.MaxInputTokens > 0 && c.Anthropic.SoftInputTokens > c.Anthropic.MaxInputTokens {
		return fmt.Errorf("anthropic.soft_input_tokens must be <= max_input_tokens")
	}
	if c.Anthropic.MaxToolResultChars < 0 || c.Anthropic.KeepRecentMessages < 0 {
		return fmt.Errorf("anthropic max_tool_result_chars/keep_recent_messages must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Level)) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level must be debug, info, warn, or error")
	}
	return c.ValidateListen(c.Listen)
}

// RequestTimeout returns the configured HTTP request timeout as a duration.
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.Limits.RequestTimeoutSec) * time.Second
}

// ValidateListen enforces loopback-first operation. Public binds require an
// explicit opt-in because the proxy stores bearer credentials and consumes quota.
func (c Config) ValidateListen(addr string) error {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("listen address %q must be host:port: %w", addr, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("listen address %q has an invalid port", addr)
	}
	if !IsPublicListen(addr) {
		return nil
	}
	if !c.AllowPublicListen {
		return fmt.Errorf("public listen %q requires allow_public_listen: true or ALLOW_PUBLIC_LISTEN=true", addr)
	}
	return nil
}

// IsPublicListen reports whether addr binds all interfaces or a non-loopback IP.
// Hostnames are treated as public because their resolution may change.
func IsPublicListen(addr string) bool {
	addr = strings.TrimSpace(addr)
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			return true
		}
		return true
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// StickyTTL returns sticky session TTL as a duration.
func (c Config) StickyTTL() time.Duration {
	return time.Duration(c.LB.StickyTTLSec) * time.Second
}

// RefreshSkew returns pre-expiry refresh skew as a duration.
func (c Config) RefreshSkew() time.Duration {
	return time.Duration(c.LB.RefreshSkewSec) * time.Second
}

// ResolveModel maps an Anthropic/Claude model id to an upstream Grok model.
// If model already matches a passthrough prefix, it is returned unchanged.
// Unknown models are returned as-is (caller may still reject).
func (c Config) ResolveModel(model string) string {
	return c.Anthropic.ResolveModel(model)
}

// ResolveModel maps an Anthropic model id using explicit aliases only.
// Unknown future model ids are not guessed because their capabilities may
// differ from the configured target.
//
// Claude Code appends a literal "[1m]" suffix to model ids when the user
// selects 1M context. That suffix only changes the client's local window
// budget; strip it before alias lookup / passthrough so upstream receives a
// real model id.
func (c AnthropicConfig) ResolveModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	model = stripClaudeCodeContextSuffix(model)
	for _, p := range c.PassthroughPrefixes {
		if p != "" && len(model) >= len(p) && model[:len(p)] == p {
			return model
		}
	}
	if alias, ok := c.ModelAliases[model]; ok && alias != "" {
		return alias
	}
	return model
}

// stripClaudeCodeContextSuffix removes Claude Code's local context-window
// marker (e.g. "claude-opus-4-6[1m]" → "claude-opus-4-6").
func stripClaudeCodeContextSuffix(model string) string {
	const suffix = "[1m]"
	if len(model) < len(suffix) {
		return model
	}
	if strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return strings.TrimSpace(model[:len(model)-len(suffix)])
	}
	return model
}

// PrefetchEnabled reports whether background token prefetch is on (default true).
func (c Config) PrefetchEnabled() bool {
	if c.LB.Prefetch.Enabled == nil {
		return true
	}
	return *c.LB.Prefetch.Enabled
}

// SoftDemoteOn429Enabled reports whether 429 soft demotion is on (default true).
func (c Config) SoftDemoteOn429Enabled() bool {
	if c.LB.SoftDemoteOn429 == nil {
		return true
	}
	return *c.LB.SoftDemoteOn429
}

// MaxAttempts returns credential failover attempts per request (default 32).
func (c Config) MaxAttempts() int {
	if c.LB.MaxAttempts > 0 {
		return c.LB.MaxAttempts
	}
	return 32
}

// FreeUsageExhaustedCooldown returns how long free-usage-exhausted accounts stay out.
func (c Config) FreeUsageExhaustedCooldown() time.Duration {
	sec := c.LB.Cooldown.FreeUsageExhaustedSec
	if sec <= 0 {
		sec = 72000 // 20h
	}
	return time.Duration(sec) * time.Second
}

// PrefetchInterval returns the prefetch scan interval.
func (c Config) PrefetchInterval() time.Duration {
	sec := c.LB.Prefetch.IntervalSec
	if sec <= 0 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

// PrefetchMaxPerTick returns max credentials refreshed per tick.
func (c Config) PrefetchMaxPerTick() int {
	n := c.LB.Prefetch.MaxPerTick
	if n <= 0 {
		return 128
	}
	return n
}

// PrefetchConcurrency returns parallel refresh workers per tick.
func (c Config) PrefetchConcurrency() int {
	n := c.LB.Prefetch.Concurrency
	if n <= 0 {
		return 16
	}
	return n
}

// AutoCompactEnabled reports whether request auto-compact is on (default true).
func (c AnthropicConfig) AutoCompactEnabled() bool {
	if c.AutoCompact == nil {
		return true
	}
	return *c.AutoCompact
}

// PreserveCacheHintsEnabled reports whether cache_control hints are preserved (default true).
func (c AnthropicConfig) PreserveCacheHintsEnabled() bool {
	if c.PreserveCacheHints == nil {
		return true
	}
	return *c.PreserveCacheHints
}

// EffectiveSoftInputTokens returns soft compact threshold with defaults.
func (c AnthropicConfig) EffectiveSoftInputTokens() int {
	if c.SoftInputTokens > 0 {
		return c.SoftInputTokens
	}
	if c.MaxInputTokens > 0 {
		// soft at 90% of hard limit when only hard is set
		return c.MaxInputTokens * 9 / 10
	}
	return 400000
}

// EffectiveMaxInputTokens returns hard reject threshold with defaults.
func (c AnthropicConfig) EffectiveMaxInputTokens() int {
	if c.MaxInputTokens > 0 {
		return c.MaxInputTokens
	}
	return 460000
}

// EffectiveMaxToolResultChars returns tool_result truncation budget.
func (c AnthropicConfig) EffectiveMaxToolResultChars() int {
	if c.MaxToolResultChars > 0 {
		return c.MaxToolResultChars
	}
	return 120000
}

// EffectiveKeepRecentMessages returns how many recent messages survive compact.
func (c AnthropicConfig) EffectiveKeepRecentMessages() int {
	if c.KeepRecentMessages > 0 {
		return c.KeepRecentMessages
	}
	return 16
}
