// Package lb implements multi-credential selection, sticky sessions and cooldown.
//
// It does not perform HTTP or token refresh — only pick / mark success|failure
// and maintain process-local runtime state.
package lb

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
)

// ErrNoCredential is returned when no enabled, non-cooling credential is available.
var ErrNoCredential = errors.New("lb: no available credential")

type healthStore interface {
	PatchCredential(id string, mutate func(*storage.Credential) error) (storage.Credential, error)
}

// deferredHealthStore optionally coalesces health patches.
type deferredHealthStore interface {
	EnqueueHealth(id string, mutate func(*storage.Credential) error)
}

const successPersistenceInterval = 30 * time.Second

type healthSnapshot struct {
	version       uint64
	failureCount  int
	cooldownUntil *time.Time
	lastError     string
	lastSuccessAt *time.Time
}

// Selector picks credentials according to strategy, sticky session and cooldown.
type Selector struct {
	strategy        string
	stickyTTL       time.Duration
	cooldownBase    time.Duration
	cooldownMax     time.Duration
	softDemoteOn429 bool

	mu        sync.Mutex
	persistMu sync.Mutex

	// rrIndex is the flat round-robin cursor (strategy=round_robin).
	rrIndex int
	// priorityRR is per-priority round-robin cursors (strategy=priority_rr).
	priorityRR map[int]int

	sticky       map[string]stickyBinding
	stickySlots  []string
	stickyCursor int
	states       map[string]*runtimeState
	store        healthStore
}

// SetHealthStore enables durable failure/cooldown state. It returns s for
// convenient dependency wiring.
func (s *Selector) SetHealthStore(store healthStore) *Selector {
	s.mu.Lock()
	s.store = store
	s.mu.Unlock()
	return s
}

// New builds a Selector from LB configuration.
func New(cfg config.LBConfig) *Selector {
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "priority_rr"
	}
	base := time.Duration(cfg.Cooldown.BaseSec) * time.Second
	max := time.Duration(cfg.Cooldown.MaxSec) * time.Second
	if base <= 0 {
		base = 300 * time.Second
	}
	if max <= 0 {
		max = 3600 * time.Second
	}
	soft := true
	if cfg.SoftDemoteOn429 != nil {
		soft = *cfg.SoftDemoteOn429
	}
	return &Selector{
		strategy:        strategy,
		stickyTTL:       time.Duration(cfg.StickyTTLSec) * time.Second,
		cooldownBase:    base,
		cooldownMax:     max,
		softDemoteOn429: soft,
		priorityRR:      make(map[int]int),
		sticky:          make(map[string]stickyBinding),
		states:          make(map[string]*runtimeState),
	}
}

// Available returns credentials that are enabled and not in cooldown (storage fields only).
func Available(creds []storage.Credential, now time.Time) []storage.Credential {
	out := make([]storage.Credential, 0, len(creds))
	for _, c := range creds {
		if !c.Enabled {
			continue
		}
		if c.CooldownUntil != nil && c.CooldownUntil.After(now) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Pick selects one usable credential.
// When stickyKey is non-empty, a live sticky binding is preferred if still available;
// otherwise a new credential is chosen and re-bound.
func (s *Selector) Pick(creds []storage.Credential, stickyKey string, now time.Time) (storage.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	avail := s.availableLocked(creds, now)
	if len(avail) == 0 {
		return storage.Credential{}, ErrNoCredential
	}

	// Sticky hit.
	if stickyKey != "" {
		if id, ok := s.getSticky(stickyKey, now); ok {
			if c, found := findByID(avail, id); found {
				return c, nil
			}
			// Bound credential no longer available — fall through and rebind.
		}
	}

	picked, err := s.pickByStrategy(avail)
	if err != nil {
		return storage.Credential{}, err
	}
	if stickyKey != "" {
		s.bindSticky(stickyKey, picked.ID, now)
	}
	return picked, nil
}

// MarkSuccess clears failure/cooldown for credID and refreshes sticky binding.
func (s *Selector) MarkSuccess(credID, stickyKey string, now time.Time) {
	if credID == "" {
		return
	}
	s.mu.Lock()
	st := s.ensureState(credID)
	needsPersist := st.FailureCount != 0 ||
		!st.CooldownUntil.IsZero() ||
		st.LastError != "" ||
		st.LastSuccessPersistedAt.IsZero() ||
		now.Before(st.LastSuccessPersistedAt) ||
		now.Sub(st.LastSuccessPersistedAt) >= successPersistenceInterval
	st.FailureCount = 0
	st.CooldownUntil = time.Time{}
	st.LastError = ""
	st.DemoteUntil = time.Time{}
	st.DemoteScore = 0

	if stickyKey != "" {
		s.bindSticky(stickyKey, credID, now)
	}
	store := s.store
	var snapshot healthSnapshot
	if needsPersist {
		successAt := now.UTC().Truncate(time.Second)
		st.LastSuccessPersistedAt = successAt
		st.Version++
		snapshot = healthSnapshot{
			version:       st.Version,
			lastSuccessAt: &successAt,
		}
	}
	s.mu.Unlock()
	if store != nil && needsPersist {
		s.persistHealth(store, credID, snapshot)
	}
}

// MarkFailure records a failure and applies cooldown based on status.
// retryAfter is honored for 429 when > 0.
func (s *Selector) MarkFailure(credID string, status int, retryAfter time.Duration, now time.Time) {
	if credID == "" {
		return
	}
	s.mu.Lock()
	st := s.ensureState(credID)
	st.FailureCount++
	d := s.cooldownDuration(status, retryAfter, st.FailureCount-1)
	st.CooldownUntil = now.Add(d)
	if status > 0 {
		st.LastError = fmt.Sprintf("http %d", status)
	} else {
		st.LastError = "network error"
	}
	if s.softDemoteOn429 && status == 429 {
		// Soft demote longer than the immediate cooldown so recovered accounts
		// still trail healthier ones for a while.
		st.DemoteScore++
		extra := d * 2
		if extra < 10*time.Minute {
			extra = 10 * time.Minute
		}
		if extra > 2*time.Hour {
			extra = 2 * time.Hour
		}
		until := now.Add(extra)
		if until.After(st.DemoteUntil) {
			st.DemoteUntil = until
		}
	}

	// Sticky bindings to a cooling credential should not keep routing traffic there.
	if d > 0 {
		s.clearStickyForCred(credID)
	}
	failureCount := st.FailureCount
	cooldownUntil := st.CooldownUntil.UTC().Truncate(time.Second)
	lastError := st.LastError
	st.Version++
	snapshot := healthSnapshot{
		version:       st.Version,
		failureCount:  failureCount,
		cooldownUntil: &cooldownUntil,
		lastError:     lastError,
	}
	store := s.store
	s.mu.Unlock()
	if store != nil {
		s.persistHealth(store, credID, snapshot)
	}
}

func (s *Selector) persistHealth(store healthStore, credID string, snapshot healthSnapshot) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	current := s.states[credID]
	stale := current == nil || current.Version != snapshot.version
	s.mu.Unlock()
	if stale {
		return
	}
	_, _ = store.PatchCredential(credID, func(c *storage.Credential) error {
		c.FailureCount = snapshot.failureCount
		c.CooldownUntil = snapshot.cooldownUntil
		c.LastError = snapshot.lastError
		if snapshot.lastSuccessAt != nil {
			c.LastSuccessAt = snapshot.lastSuccessAt
		}
		return nil
	})
}

// availableLocked filters enabled + not cooling, merging memory cooldowns.
// Caller must hold s.mu.
func (s *Selector) availableLocked(creds []storage.Credential, now time.Time) []storage.Credential {
	out := make([]storage.Credential, 0, len(creds))
	for _, c := range creds {
		if !c.Enabled {
			continue
		}
		s.seedState(c)
		if s.inCooldown(c, now) {
			continue
		}
		out = append(out, c)
	}
	// Prefer: fresh AT + not demoted, then fresh demoted, then stale, then stale demoted.
	// Stable partitions keep RR fairness inside each bucket.
	if len(out) > 1 {
		var freshOK, freshDemoted, staleOK, staleDemoted []storage.Credential
		for _, c := range out {
			demoted := s.isDemoted(c.ID, now)
			fresh := preferFreshAccess(c, now)
			switch {
			case fresh && !demoted:
				freshOK = append(freshOK, c)
			case fresh && demoted:
				freshDemoted = append(freshDemoted, c)
			case !fresh && !demoted:
				staleOK = append(staleOK, c)
			default:
				staleDemoted = append(staleDemoted, c)
			}
		}
		out = out[:0]
		out = append(out, freshOK...)
		out = append(out, freshDemoted...)
		out = append(out, staleOK...)
		out = append(out, staleDemoted...)
	}
	return out
}

// seedState restores runtime backoff from persisted health after restart.
// Caller must hold s.mu.
func (s *Selector) seedState(c storage.Credential) {
	if _, exists := s.states[c.ID]; exists {
		return
	}
	st := &runtimeState{FailureCount: c.FailureCount, LastError: c.LastError}
	if c.CooldownUntil != nil {
		st.CooldownUntil = *c.CooldownUntil
	}
	if c.LastSuccessAt != nil {
		st.LastSuccessPersistedAt = *c.LastSuccessAt
	}
	s.states[c.ID] = st
}

// pickByStrategy chooses from a non-empty available list.
// Caller must hold s.mu.
func (s *Selector) pickByStrategy(avail []storage.Credential) (storage.Credential, error) {
	if len(avail) == 0 {
		return storage.Credential{}, ErrNoCredential
	}
	switch s.strategy {
	case "round_robin":
		return s.pickRoundRobin(avail), nil
	case "priority_rr", "":
		return s.pickPriorityRR(avail), nil
	default:
		// Unknown strategy: fall back to priority_rr for safety.
		return s.pickPriorityRR(avail), nil
	}
}

// pickRoundRobin advances a flat RR cursor over avail (order preserved).
// Caller must hold s.mu.
func (s *Selector) pickRoundRobin(avail []storage.Credential) storage.Credential {
	if s.rrIndex < 0 {
		s.rrIndex = 0
	}
	idx := s.rrIndex % len(avail)
	s.rrIndex = (idx + 1) % len(avail)
	// Keep index from growing unbounded when list shrinks.
	if s.rrIndex >= len(avail) {
		s.rrIndex = 0
	}
	return avail[idx]
}

// pickPriorityRR groups by Priority desc and RR within the highest-priority group present.
// Caller must hold s.mu.
func (s *Selector) pickPriorityRR(avail []storage.Credential) storage.Credential {
	// Group by priority.
	groups := make(map[int][]storage.Credential)
	priorities := make([]int, 0)
	for _, c := range avail {
		if _, ok := groups[c.Priority]; !ok {
			priorities = append(priorities, c.Priority)
		}
		groups[c.Priority] = append(groups[c.Priority], c)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	top := priorities[0]
	group := groups[top]
	idx := s.priorityRR[top]
	if idx < 0 {
		idx = 0
	}
	idx = idx % len(group)
	s.priorityRR[top] = (idx + 1) % len(group)
	return group[idx]
}

func (s *Selector) ensureState(credID string) *runtimeState {
	st, ok := s.states[credID]
	if !ok {
		st = &runtimeState{}
		s.states[credID] = st
	}
	return st
}


// preferFreshAccess reports whether c currently has a non-expired access token.
func (s *Selector) isDemoted(credID string, now time.Time) bool {
	if s == nil || !s.softDemoteOn429 || credID == "" {
		return false
	}
	st, ok := s.states[credID]
	if !ok || st == nil {
		return false
	}
	return !st.DemoteUntil.IsZero() && st.DemoteUntil.After(now)
}

func preferFreshAccess(c storage.Credential, now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return c.ExpiresAt.After(now)
}

func findByID(creds []storage.Credential, id string) (storage.Credential, bool) {
	for _, c := range creds {
		if c.ID == id {
			return c, true
		}
	}
	return storage.Credential{}, false
}
