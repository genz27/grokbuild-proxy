package lb

import (
	"math/rand"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
)

// runtimeState is in-process failure / cooldown state for a credential.
type runtimeState struct {
	FailureCount           int
	CooldownUntil          time.Time
	LastError              string
	LastSuccessPersistedAt time.Time
	// Soft demotion after 429: lower pick preference without full cooldown exclusion.
	DemoteUntil time.Time
	DemoteScore int
	Version     uint64
}

// cooldownDuration derives a cooldown window from HTTP status and history.
//
// Rules:
//   - 429: Retry-After if > 0 (exact, no jitter), else base * 2^failures capped at max + jitter
//   - 401/402/403: longer cooldown (402/403 use max; 402 honors Retry-After exactly)
//   - 5xx: short cooldown (~base/10, floor 15s) + jitter
//   - other: base + jitter
//
// Explicit retryAfter windows (server Retry-After or free-usage exhaustion) are
// honored as-is so a 20h quota reset is not randomly stretched to ~22h.
func (s *Selector) cooldownDuration(status int, retryAfter time.Duration, failures int) time.Duration {
	base := s.cooldownBase
	max := s.cooldownMax
	if base <= 0 {
		base = 300 * time.Second
	}
	if max <= 0 {
		max = 3600 * time.Second
	}
	if max < base {
		max = base
	}

	var d time.Duration
	exact := false
	switch {
	case status == 429:
		if retryAfter > 0 {
			d = retryAfter
			exact = true
		} else {
			shift := failures
			if shift < 0 {
				shift = 0
			}
			if shift > 10 {
				shift = 10
			}
			d = base * time.Duration(uint(1)<<uint(shift))
			if d > max || d <= 0 {
				d = max
			}
		}
	case status == 402:
		if retryAfter > 0 {
			d = retryAfter
			exact = true
		} else {
			d = max
		}
	case status == 403:
		d = max
	case status == 401:
		d = base * 4
		if d > max {
			d = max
		}
	case status >= 500 && status <= 599:
		d = base / 10
		if d < 15*time.Second {
			d = 15 * time.Second
		}
		if d > base {
			d = base
		}
	default:
		d = base
	}

	if exact {
		return d
	}
	return d + jitter(d)
}

// jitter returns a small positive random duration in [0, ~10% of d].
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// 0 .. d/10 inclusive-ish
	span := int64(d / 10)
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(span + 1))
}

// effectiveCooldown returns the later of storage and in-memory cooldown ends.
func (s *Selector) effectiveCooldown(c storage.Credential) time.Time {
	var until time.Time
	if c.CooldownUntil != nil && !c.CooldownUntil.IsZero() {
		until = *c.CooldownUntil
	}
	if st, ok := s.states[c.ID]; ok && st.CooldownUntil.After(until) {
		until = st.CooldownUntil
	}
	return until
}

// inCooldown reports whether the credential is cooling down at now.
func (s *Selector) inCooldown(c storage.Credential, now time.Time) bool {
	until := s.effectiveCooldown(c)
	return !until.IsZero() && until.After(now)
}

// ApplyCooldownToCredential copies in-memory cooldown / failure state onto c
// so callers can persist it via storage. No-op when no runtime state exists.
func (s *Selector) ApplyCooldownToCredential(c *storage.Credential) {
	if c == nil || c.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[c.ID]
	if !ok {
		return
	}
	c.FailureCount = st.FailureCount
	c.LastError = st.LastError
	if st.CooldownUntil.IsZero() {
		c.CooldownUntil = nil
	} else {
		t := st.CooldownUntil.UTC()
		c.CooldownUntil = &t
	}
}
