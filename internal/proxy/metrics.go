package proxy

import (
	"sync/atomic"
	"time"
)

// PathMetrics is a low-cardinality snapshot of executor hot-path timings.
type PathMetrics struct {
	ListCredsCalls   uint64  `json:"list_creds_calls"`
	ListCredsTotalMS float64 `json:"list_creds_total_ms"`
	PickCalls        uint64  `json:"pick_calls"`
	PickTotalMS      float64 `json:"pick_total_ms"`
	RefreshCalls     uint64  `json:"refresh_calls"`
	RefreshTotalMS   float64 `json:"refresh_total_ms"`
	RefreshErrors    uint64  `json:"refresh_errors"`
	UpstreamCalls    uint64  `json:"upstream_calls"`
	UpstreamTotalMS  float64 `json:"upstream_total_ms"`
	TTFTSamples      uint64  `json:"ttft_samples"`
	TTFTTotalMS      float64 `json:"ttft_total_ms"`
	Failovers        uint64  `json:"failovers"`
}

type pathCounters struct {
	listCalls     atomic.Uint64
	listNanos     atomic.Uint64
	pickCalls     atomic.Uint64
	pickNanos     atomic.Uint64
	refreshCalls  atomic.Uint64
	refreshNanos  atomic.Uint64
	refreshErrors atomic.Uint64
	upstreamCalls atomic.Uint64
	upstreamNanos atomic.Uint64
	ttftSamples   atomic.Uint64
	ttftNanos     atomic.Uint64
	failovers     atomic.Uint64
}

func (e *Executor) PathSnapshot() PathMetrics {
	if e == nil {
		return PathMetrics{}
	}
	toMS := func(n uint64) float64 { return float64(n) / float64(time.Millisecond) }
	return PathMetrics{
		ListCredsCalls:   e.path.listCalls.Load(),
		ListCredsTotalMS: toMS(e.path.listNanos.Load()),
		PickCalls:        e.path.pickCalls.Load(),
		PickTotalMS:      toMS(e.path.pickNanos.Load()),
		RefreshCalls:     e.path.refreshCalls.Load(),
		RefreshTotalMS:   toMS(e.path.refreshNanos.Load()),
		RefreshErrors:    e.path.refreshErrors.Load(),
		UpstreamCalls:    e.path.upstreamCalls.Load(),
		UpstreamTotalMS:  toMS(e.path.upstreamNanos.Load()),
		TTFTSamples:      e.path.ttftSamples.Load(),
		TTFTTotalMS:      toMS(e.path.ttftNanos.Load()),
		Failovers:        e.path.failovers.Load(),
	}
}

func (e *Executor) observeList(d time.Duration) {
	if e == nil {
		return
	}
	e.path.listCalls.Add(1)
	e.path.listNanos.Add(uint64(d))
}

func (e *Executor) observePick(d time.Duration) {
	if e == nil {
		return
	}
	e.path.pickCalls.Add(1)
	e.path.pickNanos.Add(uint64(d))
}

func (e *Executor) observeRefresh(ok bool, d time.Duration) {
	if e == nil {
		return
	}
	e.path.refreshCalls.Add(1)
	e.path.refreshNanos.Add(uint64(d))
	if !ok {
		e.path.refreshErrors.Add(1)
	}
}

func (e *Executor) observeUpstream(d time.Duration) {
	if e == nil {
		return
	}
	e.path.upstreamCalls.Add(1)
	e.path.upstreamNanos.Add(uint64(d))
}

func (e *Executor) observeTTFT(d time.Duration) {
	if e == nil || d <= 0 {
		return
	}
	e.path.ttftSamples.Add(1)
	e.path.ttftNanos.Add(uint64(d))
}

func (e *Executor) observeFailover() {
	if e == nil {
		return
	}
	e.path.failovers.Add(1)
}
