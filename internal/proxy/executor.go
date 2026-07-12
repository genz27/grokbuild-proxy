// Package proxy provides the multi-credential upstream executor used by OpenAI/Anthropic handlers.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/auth"
	"github.com/GreyGunG/grokbuild-proxy/internal/lb"
	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
	"github.com/GreyGunG/grokbuild-proxy/internal/upstream"
)

// DefaultMaxAttempts is the max number of credential picks for a single Post.
// High enough to walk a free-tier pool past exhausted accounts without
// surfacing 429 to clients when other credentials remain healthy.
const DefaultMaxAttempts = 32

// ErrUpgradeRequired is returned when upstream responds 426 (protocol upgrade required).
var ErrUpgradeRequired = errors.New("proxy: upstream requires protocol upgrade (426)")

// Store is the subset of storage used by the executor.
type Store interface {
	ListCredentials() ([]storage.Credential, error)
	GetCredential(id string) (storage.Credential, error)
	UpdateCredential(c storage.Credential) (storage.Credential, error)
	// PatchCredential applies a mutation under a single store lock (preferred for concurrent updates).
	PatchCredential(id string, mutate func(*storage.Credential) error) (storage.Credential, error)
}

// Selector is the subset of lb.Selector used by the executor.
type Selector interface {
	Pick(creds []storage.Credential, stickyKey string, now time.Time) (storage.Credential, error)
	MarkSuccess(credID, stickyKey string, now time.Time)
	MarkFailure(credID string, status int, retryAfter time.Duration, now time.Time)
}

// Upstream is the subset of upstream.Client used by the executor.
type Upstream interface {
	PostResponses(ctx context.Context, body any, opts upstream.PostResponsesOptions) (*http.Response, error)
	ListModels(ctx context.Context, accessToken string) (*upstream.ModelList, error)
	GetBilling(ctx context.Context, accessToken string) (*upstream.MonthlyBilling, error)
	GetBillingSnapshot(ctx context.Context, accessToken string) (*upstream.BillingSnapshot, error)
}

// TokenRefresher is the subset of auth.Refresher used by the executor.
type TokenRefresher interface {
	EnsureAccess(ctx context.Context, key string, current auth.TokenSet, persist auth.TokenPersistFunc) (auth.TokenSet, error)
	ForceRefresh(ctx context.Context, key string, current auth.TokenSet, persist auth.TokenPersistFunc) (auth.TokenSet, error)
}

// Executor selects credentials, refreshes tokens, and posts to upstream /v1/responses.
type usageQueue interface {
	EnqueueLastUsed(id string, at time.Time)
}

type Executor struct {
	Store     Store
	Selector  Selector
	Upstream  Upstream
	Refresher TokenRefresher
	// UsageQueue optionally defers last_used_at writes.
	UsageQueue usageQueue
	// MaxAttempts caps credential failover. Zero uses DefaultMaxAttempts.
	MaxAttempts int
	// FreeUsageExhaustedCooldown is applied when upstream reports free quota
	// exhaustion (rolling ~24h window). Zero uses 20h.
	FreeUsageExhaustedCooldown time.Duration
	// Now is optional clock injection for tests.
	Now func() time.Time
	// Logger receives credential-selection outcomes without request bodies/tokens.
	Logger *slog.Logger
	// RequestID extracts a correlation ID from ctx.
	RequestID func(context.Context) string

	usageMu  sync.Mutex
	lastUsed map[string]time.Time
	path     pathCounters
}

// Post implements openai.PostResponsesFunc / anthropic.PostResponsesFunc.
//
// It may switch credentials on 401/429/5xx only before the response is returned
// to the caller (body not yet delivered). After a successful 2xx, MarkSuccess is
// recorded. 426 is never failed-over; the original response is returned.
func (e *Executor) Post(ctx context.Context, model, convID string, body []byte, stream bool) (*http.Response, error) {
	if e == nil {
		return nil, fmt.Errorf("proxy: nil executor")
	}
	if e.Store == nil || e.Selector == nil || e.Upstream == nil {
		return nil, fmt.Errorf("proxy: executor not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	maxAttempts := e.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}

	tried := make(map[string]struct{})
	var lastErr error
	var lastResp *http.Response
	idempotencyKey := newIdempotencyKey()
	postStart := time.Now()
	listStart := time.Now()
	creds, err := e.Store.ListCredentials()
	e.observeList(time.Since(listStart))
	if err != nil {
		return nil, fmt.Errorf("proxy: list credentials: %w", err)
	}
	if maxAttempts > len(creds) {
		maxAttempts = len(creds)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Exclude already-tried credentials from this request.
		filtered := make([]storage.Credential, 0, len(creds))
		for _, c := range creds {
			if _, ok := tried[c.ID]; ok {
				continue
			}
			filtered = append(filtered, c)
		}

		now := e.now()
		pickStart := time.Now()
		cred, err := e.Selector.Pick(filtered, convID, now)
		e.observePick(time.Since(pickStart))
		if err != nil {
			if lastResp != nil {
				return lastResp, nil
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		tried[cred.ID] = struct{}{}
		e.log(ctx, slog.LevelDebug, "credential_selected",
			"credential_id", cred.ID,
			"attempt", attempt+1,
		)

		tokenStart := time.Now()
		prevAccess := strings.TrimSpace(cred.AccessToken)
		prevRefresh := strings.TrimSpace(cred.RefreshToken)
		prevExp := cred.ExpiresAt
		tokens, err := e.EnsureToken(ctx, cred)
		e.observeRefresh(err == nil, time.Since(tokenStart))
		if err != nil {
			lastErr = err
			e.log(ctx, slog.LevelWarn, "credential_token_failed",
				"credential_id", cred.ID,
				"attempt", attempt+1,
				"error", err,
			)
			// Only cool down if store still has the same (failed) refresh material;
			// a concurrent refresh may already have rotated tokens successfully.
			if latest, gerr := e.Store.GetCredential(cred.ID); gerr == nil {
				if strings.TrimSpace(latest.RefreshToken) != "" &&
					strings.TrimSpace(latest.RefreshToken) != strings.TrimSpace(cred.RefreshToken) {
					delete(tried, cred.ID)
					continue
				}
			}
			e.Selector.MarkFailure(cred.ID, http.StatusUnauthorized, 0, e.now())
			continue
		}
		// Apply token fields from EnsureToken result. Only hit storage again when
		// a refresh rotated material (avoids GetCredential on the hot path).
		refreshed := strings.TrimSpace(tokens.AccessToken) != prevAccess ||
			(strings.TrimSpace(tokens.RefreshToken) != "" && strings.TrimSpace(tokens.RefreshToken) != prevRefresh) ||
			(!tokens.ExpiresAt.IsZero() && !tokens.ExpiresAt.Equal(prevExp))
		if refreshed {
			if latest, gerr := e.Store.GetCredential(cred.ID); gerr == nil {
				cred = latest
			}
		}
		cred.AccessToken = tokens.AccessToken
		if tokens.RefreshToken != "" {
			cred.RefreshToken = tokens.RefreshToken
		}
		if !tokens.ExpiresAt.IsZero() {
			cred.ExpiresAt = tokens.ExpiresAt
		}

		upStart := time.Now()
		resp, err := e.Upstream.PostResponses(ctx, body, upstream.PostResponsesOptions{
			AccessToken:  tokens.AccessToken,
			Model:        model,
			ConvID:       convID,
			Stream:       stream,
			ExtraHeaders: idempotencyHeaders(idempotencyKey),
		})
		e.observeUpstream(time.Since(upStart))
		if err != nil {
			lastErr = err
			e.log(ctx, slog.LevelWarn, "upstream_request_failed",
				"credential_id", cred.ID,
				"attempt", attempt+1,
				"error", err,
			)
			e.Selector.MarkFailure(cred.ID, 0, 0, e.now())
			continue
		}

		// 426: do not failover; return original response (or typed error if nil).
		if resp.StatusCode == http.StatusUpgradeRequired {
			return resp, nil
		}

		// Success path.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			e.observeTTFT(time.Since(postStart))
			e.log(ctx, slog.LevelDebug, "upstream_request_succeeded",
				"credential_id", cred.ID,
				"attempt", attempt+1,
				"upstream_status", resp.StatusCode,
				"ttft_ms", float64(time.Since(postStart).Microseconds())/1000,
			)
			e.Selector.MarkSuccess(cred.ID, convID, e.now())
			_ = e.touchLastUsed(cred)
			return resp, nil
		}

		// 401: force refresh once on the same credential, then retry once.
		if resp.StatusCode == http.StatusUnauthorized {
			unauthorizedResp := bufferErrorResponse(resp)
			refreshed, rerr := e.forceRefresh(ctx, cred)
			if rerr != nil {
				lastErr = rerr
				lastResp = unauthorizedResp
				e.Selector.MarkFailure(cred.ID, http.StatusUnauthorized, 0, e.now())
				continue
			}
			retry, rerr := e.Upstream.PostResponses(ctx, body, upstream.PostResponsesOptions{
				AccessToken:  refreshed.AccessToken,
				Model:        model,
				ConvID:       convID,
				Stream:       stream,
				ExtraHeaders: idempotencyHeaders(idempotencyKey),
			})
			if rerr != nil {
				lastErr = rerr
				e.Selector.MarkFailure(cred.ID, http.StatusUnauthorized, 0, e.now())
				continue
			}
			if retry.StatusCode >= 200 && retry.StatusCode < 300 {
				e.Selector.MarkSuccess(cred.ID, convID, e.now())
				_ = e.touchLastUsed(cred)
				return retry, nil
			}
			if retry.StatusCode == http.StatusUpgradeRequired {
				return retry, nil
			}
			// Still failing after refresh → mark and switch credentials.
			ra := parseRetryAfterAt(retry.Header.Get("Retry-After"), e.now())
			status := retry.StatusCode
			lastResp = bufferErrorResponse(retry)
			freeExhausted := isFreeUsageExhaustedResponse(lastResp)
			if freeExhausted {
				if freeCD := e.freeUsageExhaustedDuration(); freeCD > ra {
					ra = freeCD
				}
			}
			e.Selector.MarkFailure(cred.ID, status, ra, e.now())
			e.log(ctx, slog.LevelWarn, "upstream_retryable_status",
				"credential_id", cred.ID,
				"attempt", attempt+1,
				"upstream_status", status,
				"retry_after_ms", ra.Milliseconds(),
				"free_usage_exhausted", freeExhausted,
			)
			lastErr = fmt.Errorf("proxy: upstream status %d after refresh", status)
			e.observeFailover()
			continue
		}

		// Retryable statuses before body delivery: 429 / 5xx / 403 / 402.
		if isRetryableStatus(resp.StatusCode) {
			ra := parseRetryAfterAt(resp.Header.Get("Retry-After"), e.now())
			status := resp.StatusCode
			lastResp = bufferErrorResponse(resp)
			freeExhausted := isFreeUsageExhaustedResponse(lastResp)
			if freeExhausted {
				if freeCD := e.freeUsageExhaustedDuration(); freeCD > ra {
					ra = freeCD
				}
			}
			e.Selector.MarkFailure(cred.ID, status, ra, e.now())
			e.log(ctx, slog.LevelWarn, "upstream_retryable_status",
				"credential_id", cred.ID,
				"attempt", attempt+1,
				"upstream_status", status,
				"retry_after_ms", ra.Milliseconds(),
				"free_usage_exhausted", freeExhausted,
			)
			lastErr = fmt.Errorf("proxy: upstream status %d", status)
			e.observeFailover()
			continue
		}

		// Non-retryable error: return as-is for the handler to map.
		return resp, nil
	}

	if lastResp != nil {
		return lastResp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, lb.ErrNoCredential
}

// EnsureToken ensures a non-expired access token for the given credential,
// persisting rotated tokens via Store.UpdateCredential.
func (e *Executor) EnsureToken(ctx context.Context, cred storage.Credential) (auth.TokenSet, error) {
	if e == nil || e.Refresher == nil {
		// No refresher: return stored tokens as-is.
		return auth.TokenSet{
			AccessToken:  cred.AccessToken,
			RefreshToken: cred.RefreshToken,
			ExpiresAt:    cred.ExpiresAt,
			TokenType:    "Bearer",
		}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	current := auth.TokenSet{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		ExpiresAt:    cred.ExpiresAt,
		TokenType:    "Bearer",
	}
	return e.Refresher.EnsureAccess(ctx, cred.ID, current, e.persistFunc(cred.ID))
}

// EnsureTokenByID loads a credential and ensures a valid access token.
func (e *Executor) EnsureTokenByID(ctx context.Context, credID string) (auth.TokenSet, storage.Credential, error) {
	if e == nil || e.Store == nil {
		return auth.TokenSet{}, storage.Credential{}, fmt.Errorf("proxy: executor not configured")
	}
	cred, err := e.Store.GetCredential(credID)
	if err != nil {
		return auth.TokenSet{}, storage.Credential{}, err
	}
	prevAccess := strings.TrimSpace(cred.AccessToken)
	prevRefresh := strings.TrimSpace(cred.RefreshToken)
	ts, err := e.EnsureToken(ctx, cred)
	if err != nil {
		return auth.TokenSet{}, cred, err
	}
	if strings.TrimSpace(ts.AccessToken) != prevAccess ||
		(strings.TrimSpace(ts.RefreshToken) != "" && strings.TrimSpace(ts.RefreshToken) != prevRefresh) {
		if latest, gerr := e.Store.GetCredential(credID); gerr == nil {
			cred = latest
		}
	}
	cred.AccessToken = ts.AccessToken
	if ts.RefreshToken != "" {
		cred.RefreshToken = ts.RefreshToken
	}
	if !ts.ExpiresAt.IsZero() {
		cred.ExpiresAt = ts.ExpiresAt
	}
	return ts, cred, nil
}

// ForceRefreshToken forces an OAuth refresh for admin use.
func (e *Executor) ForceRefreshToken(ctx context.Context, credID string) (auth.TokenSet, storage.Credential, error) {
	if e == nil || e.Store == nil {
		return auth.TokenSet{}, storage.Credential{}, fmt.Errorf("proxy: executor not configured")
	}
	cred, err := e.Store.GetCredential(credID)
	if err != nil {
		return auth.TokenSet{}, storage.Credential{}, err
	}
	ts, err := e.forceRefresh(ctx, cred)
	if err != nil {
		return auth.TokenSet{}, cred, err
	}
	if latest, gerr := e.Store.GetCredential(credID); gerr == nil {
		cred = latest
	}
	return ts, cred, nil
}

// ListModels picks any usable credential, ensures a token, and lists upstream models.
func (e *Executor) ListModels(ctx context.Context) (*upstream.ModelList, error) {
	ts, _, err := e.anyAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	return e.Upstream.ListModels(ctx, ts.AccessToken)
}

// GetBillingSnapshot fetches billing for a specific credential id.
func (e *Executor) GetBillingSnapshot(ctx context.Context, credID string) (*upstream.BillingSnapshot, error) {
	ts, _, err := e.EnsureTokenByID(ctx, credID)
	if err != nil {
		return nil, err
	}
	if e.Upstream == nil {
		return nil, fmt.Errorf("proxy: upstream not configured")
	}
	return e.Upstream.GetBillingSnapshot(ctx, ts.AccessToken)
}

func (e *Executor) anyAccessToken(ctx context.Context) (auth.TokenSet, storage.Credential, error) {
	if e == nil || e.Store == nil || e.Selector == nil {
		return auth.TokenSet{}, storage.Credential{}, fmt.Errorf("proxy: executor not configured")
	}
	creds, err := e.Store.ListCredentials()
	if err != nil {
		return auth.TokenSet{}, storage.Credential{}, err
	}
	cred, err := e.Selector.Pick(creds, "", e.now())
	if err != nil {
		return auth.TokenSet{}, storage.Credential{}, err
	}
	ts, err := e.EnsureToken(ctx, cred)
	if err != nil {
		return auth.TokenSet{}, cred, err
	}
	return ts, cred, nil
}

func (e *Executor) forceRefresh(ctx context.Context, cred storage.Credential) (auth.TokenSet, error) {
	if e.Refresher == nil {
		return auth.TokenSet{}, fmt.Errorf("proxy: refresher not configured")
	}
	current := auth.TokenSet{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		ExpiresAt:    cred.ExpiresAt,
		TokenType:    "Bearer",
	}
	return e.Refresher.ForceRefresh(ctx, cred.ID, current, e.persistFunc(cred.ID))
}

func (e *Executor) persistFunc(credID string) auth.TokenPersistFunc {
	return func(ctx context.Context, next auth.TokenSet) error {
		if e.Store == nil {
			return fmt.Errorf("proxy: store not configured")
		}
		// Atomic field patch: never rewrite tokens from a stale full snapshot.
		_, err := e.Store.PatchCredential(credID, func(c *storage.Credential) error {
			c.AccessToken = next.AccessToken
			if strings.TrimSpace(next.RefreshToken) != "" {
				c.RefreshToken = next.RefreshToken
			}
			if !next.ExpiresAt.IsZero() {
				c.ExpiresAt = next.ExpiresAt
			}
			return nil
		})
		return err
	}
}

func (e *Executor) touchLastUsed(cred storage.Credential) error {
	if e.Store == nil || cred.ID == "" {
		return nil
	}
	now := e.now().UTC().Truncate(time.Second)
	e.usageMu.Lock()
	if e.lastUsed == nil {
		e.lastUsed = make(map[string]time.Time)
	}
	previous := e.lastUsed[cred.ID]
	if cred.LastUsedAt != nil && cred.LastUsedAt.After(previous) {
		previous = *cred.LastUsedAt
	}
	if !previous.IsZero() && (now.Before(previous) || now.Sub(previous) < 30*time.Second) {
		e.usageMu.Unlock()
		return nil
	}
	e.lastUsed[cred.ID] = now
	e.usageMu.Unlock()
	if e.UsageQueue != nil {
		e.UsageQueue.EnqueueLastUsed(cred.ID, now)
		return nil
	}
	// Fallback immediate patch.
	_, err := e.Store.PatchCredential(cred.ID, func(c *storage.Credential) error {
		c.LastUsedAt = &now
		return nil
	})
	if err != nil {
		e.usageMu.Lock()
		if e.lastUsed[cred.ID].Equal(now) {
			delete(e.lastUsed, cred.ID)
		}
		e.usageMu.Unlock()
	}
	return err
}

func (e *Executor) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) log(ctx context.Context, level slog.Level, message string, args ...any) {
	if e == nil || e.Logger == nil {
		return
	}
	if e.RequestID != nil {
		if requestID := e.RequestID(ctx); requestID != "" {
			args = append([]any{"request_id", requestID}, args...)
		}
	}
	e.Logger.Log(ctx, level, message, args...)
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusPaymentRequired, http.StatusTooManyRequests, http.StatusForbidden:
		return true
	default:
		return code >= 500 && code <= 599
	}
}

// parseRetryAfter parses a Retry-After header (seconds or HTTP-date). Zero if unknown.
func parseRetryAfter(v string) time.Duration {
	return parseRetryAfterAt(v, time.Now())
}

func parseRetryAfterAt(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil {
		if sec < 0 {
			return 0
		}
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

func newIdempotencyKey() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return "grokbuild-" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("grokbuild-%d", time.Now().UnixNano())
}

func idempotencyHeaders(key string) http.Header {
	headers := make(http.Header)
	headers.Set("Idempotency-Key", key)
	headers.Set("X-Idempotency-Key", key)
	return headers
}

func bufferErrorResponse(resp *http.Response) *http.Response {
	if resp == nil {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	clone := new(http.Response)
	*clone = *resp
	clone.Header = resp.Header.Clone()
	clone.Body = io.NopCloser(strings.NewReader(string(raw)))
	clone.ContentLength = int64(len(raw))
	return clone
}


func (e *Executor) freeUsageExhaustedDuration() time.Duration {
	if e != nil && e.FreeUsageExhaustedCooldown > 0 {
		return e.FreeUsageExhaustedCooldown
	}
	return 20 * time.Hour
}

// isFreeUsageExhaustedResponse reports whether an upstream error body indicates
// free-tier quota exhaustion (rolling ~24h window), not a short rate limit.
func isFreeUsageExhaustedResponse(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && len(raw) == 0 {
		return false
	}
	resp.Body = io.NopCloser(strings.NewReader(string(raw)))
	resp.ContentLength = int64(len(raw))
	return isFreeUsageExhaustedBody(string(raw))
}

func isFreeUsageExhaustedBody(body string) bool {
	lower := strings.ToLower(body)
	// Prefer explicit free-usage markers; avoid matching short rate-limit 429s.
	markers := []string{
		"free_usage_exhausted",
		"free usage exhausted",
		"free-usage-exhausted",
		"free_tier_usage_exhausted",
		"free tier usage exhausted",
		"included free usage",
		"subscription:free-usage-exhausted",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// DrainAndClose is a helper for callers that abandon a response.
func DrainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
