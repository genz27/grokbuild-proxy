package proxy

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
)

// Prefetcher proactively refreshes access tokens that are near expiry so the
// request path rarely pays an OAuth round-trip on first use.
type Prefetcher struct {
	Store    Store
	Executor *Executor
	// Interval between full scans. Zero defaults to 30s.
	Interval time.Duration
	// Skew is how early a token is considered needing refresh. Zero uses 3m.
	Skew time.Duration
	// MaxPerTick caps how many credentials are refreshed in one scan.
	MaxPerTick int
	// Concurrency bounds parallel refresh workers per tick.
	Concurrency int
	Logger      *slog.Logger

	stop     chan struct{}
	once     sync.Once
	cursorMu sync.Mutex
	cursor   int
}

// Start launches the background loop. Safe to call once.
func (p *Prefetcher) Start() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.stop = make(chan struct{})
		go p.loop()
	})
}

// Stop ends the background loop.
func (p *Prefetcher) Stop() {
	if p == nil || p.stop == nil {
		return
	}
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

func (p *Prefetcher) loop() {
	interval := p.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-timer.C:
			p.tick()
			timer.Reset(interval)
		}
	}
}

func (p *Prefetcher) tick() {
	if p.Store == nil || p.Executor == nil || p.Executor.Refresher == nil {
		return
	}
	creds, err := p.Store.ListCredentials()
	if err != nil {
		p.log(slog.LevelWarn, "prefetch_list_failed", "error", err)
		return
	}
	now := time.Now()
	skew := p.Skew
	if skew <= 0 {
		skew = 3 * time.Minute
	}
	maxN := p.MaxPerTick
	if maxN <= 0 {
		maxN = 128
	}
	workers := p.Concurrency
	if workers <= 0 {
		workers = 16
	}

	candidates := make([]storage.Credential, 0, maxN)
	p.cursorMu.Lock()
	start := p.cursor
	if start < 0 || start >= len(creds) {
		start = 0
	}
	p.cursorMu.Unlock()
	last := start
	for scanned := 0; scanned < len(creds); scanned++ {
		index := (start + scanned) % len(creds)
		c := creds[index]
		last = index
		if !c.Enabled {
			continue
		}
		if strings.TrimSpace(c.RefreshToken) == "" {
			continue
		}
		if c.CooldownUntil != nil && c.CooldownUntil.After(now) {
			continue
		}
		if strings.TrimSpace(c.AccessToken) != "" && !c.ExpiresAt.IsZero() && c.ExpiresAt.After(now.Add(skew)) {
			continue
		}
		candidates = append(candidates, c)
		if len(candidates) >= maxN {
			break
		}
	}
	if len(creds) > 0 {
		p.cursorMu.Lock()
		p.cursor = (last + 1) % len(creds)
		p.cursorMu.Unlock()
	}
	if len(candidates) == 0 {
		return
	}

	p.log(slog.LevelInfo, "prefetch_tick", "candidates", len(candidates), "workers", workers)

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, cred := range candidates {
		cred := cred
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			start := time.Now()
			_, err := p.Executor.EnsureToken(ctx, cred)
			elapsed := time.Since(start)
			p.Executor.observeRefresh(err == nil, elapsed)
			if err != nil {
				p.log(slog.LevelWarn, "prefetch_refresh_failed",
					"credential_id", cred.ID,
					"duration_ms", float64(elapsed.Microseconds())/1000,
					"error", err,
				)
				return
			}
			p.log(slog.LevelDebug, "prefetch_refresh_ok",
				"credential_id", cred.ID,
				"duration_ms", float64(elapsed.Microseconds())/1000,
			)
		}()
	}
	wg.Wait()
}

func (p *Prefetcher) log(level slog.Level, msg string, args ...any) {
	if p == nil || p.Logger == nil {
		return
	}
	p.Logger.Log(context.Background(), level, msg, args...)
}
