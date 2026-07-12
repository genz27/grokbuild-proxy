// Package upstream talks to cli-chat-proxy.grok.com (Grok Build subscription path).
package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Grok Build CLI chat proxy base.
	DefaultBaseURL = "https://cli-chat-proxy.grok.com/v1"
	// DefaultClientVersion mirrors a recent Grok CLI release.
	DefaultClientVersion = "0.2.93"
	// DefaultClientIdentifier is the x-grok-client-identifier value.
	DefaultClientIdentifier = "grok-pager"
	// DefaultTokenAuth is the X-XAI-Token-Auth header value.
	DefaultTokenAuth = "xai-grok-cli"
	// DefaultUserAgent mimics the official CLI.
	DefaultUserAgent = "grok-pager/0.2.93 grok-shell/0.2.93 (linux; x86_64)"
)

// Config holds upstream client defaults.
type Config struct {
	BaseURL          string
	ClientVersion    string
	ClientIdentifier string
	TokenAuth        string
	UserAgent        string
	// RequestTimeout bounds connection establishment and response headers.
	// Streaming bodies remain governed by the request context.
	RequestTimeout time.Duration
	// HTTPClient overrides the transport. When nil, a sensible default is used.
	HTTPClient *http.Client
}

// Client is a thin HTTP client for cli-chat-proxy.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient builds a Client with defaults applied.
func NewClient(cfg Config) *Client {
	cfg = normalizeConfig(cfg)
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.RequestTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{
			Timeout: 0, // per-request context controls deadlines; streams must not have a client-wide timeout
			Transport: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				ForceAttemptHTTP2: true,
				// Sized for thousands of concurrent SSE streams against one upstream host.
				MaxIdleConns:          4096,
				MaxIdleConnsPerHost:   2048,
				MaxConnsPerHost:       0,
				IdleConnTimeout:       120 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: timeout,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}
	return &Client{cfg: cfg, http: hc}
}

// Config returns a copy of the active config.
func (c *Client) Config() Config {
	if c == nil {
		return Config{}
	}
	return c.cfg
}

// BaseURL returns the normalized base URL (no trailing slash).
func (c *Client) BaseURL() string {
	if c == nil {
		return DefaultBaseURL
	}
	return c.cfg.BaseURL
}

// RequestOptions customizes a single upstream call.
type RequestOptions struct {
	AccessToken string
	Model       string
	ConvID      string
	// ExtraHeaders are applied after defaults (can override).
	ExtraHeaders http.Header
	// Accept overrides the Accept header. Empty keeps method defaults.
	Accept string
	// ContentType overrides Content-Type for requests with a body.
	ContentType string
}

// NewRequest builds an upstream request with standard Grok CLI headers.
// body may be nil. The returned request is not yet executed.
func (c *Client) NewRequest(ctx context.Context, method, path string, body io.Reader, opts RequestOptions) (*http.Request, error) {
	if c == nil {
		return nil, fmt.Errorf("upstream: nil client")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u := joinURL(c.cfg.BaseURL, path)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("upstream: create request: %w", err)
	}
	ApplyHeaders(req, HeaderInput{
		AccessToken:      opts.AccessToken,
		Model:            opts.Model,
		ConvID:           opts.ConvID,
		ClientVersion:    c.cfg.ClientVersion,
		ClientIdentifier: c.cfg.ClientIdentifier,
		TokenAuth:        c.cfg.TokenAuth,
		UserAgent:        c.cfg.UserAgent,
		Accept:           opts.Accept,
		ContentType:      opts.ContentType,
		Extra:            opts.ExtraHeaders,
	})
	return req, nil
}

// Do executes a pre-built request. Callers that stream must Close the body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("upstream: nil client")
	}
	if req == nil {
		return nil, fmt.Errorf("upstream: nil request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: do: %w", err)
	}
	return resp, nil
}

// DoJSON is a convenience for non-stream JSON requests. It fully consumes and
// closes the response body, returning status, headers, and body bytes.
func (c *Client) DoJSON(ctx context.Context, method, path string, body io.Reader, opts RequestOptions) (status int, header http.Header, raw []byte, err error) {
	if opts.Accept == "" {
		opts.Accept = "application/json"
	}
	if body != nil && opts.ContentType == "" {
		opts.ContentType = "application/json"
	}
	req, err := c.NewRequest(ctx, method, path, body, opts)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, resp.Header.Clone(), nil, fmt.Errorf("upstream: read body: %w", err)
	}
	return resp.StatusCode, resp.Header.Clone(), raw, nil
}

func normalizeConfig(cfg Config) Config {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if strings.TrimSpace(cfg.ClientVersion) == "" {
		cfg.ClientVersion = DefaultClientVersion
	}
	if strings.TrimSpace(cfg.ClientIdentifier) == "" {
		cfg.ClientIdentifier = DefaultClientIdentifier
	}
	if strings.TrimSpace(cfg.TokenAuth) == "" {
		cfg.TokenAuth = DefaultTokenAuth
	}
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	return cfg
}

func joinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	path = strings.TrimSpace(path)
	if path == "" {
		return base
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}
