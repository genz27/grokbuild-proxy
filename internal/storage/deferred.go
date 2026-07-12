package storage

import (
	"fmt"
	"sync"
	"time"
)

// DeferredPatcher coalesces many credential field patches into occasional
// single-file rewrites, cutting lock hold time under high QPS.
type DeferredPatcher struct {
	Store *Store

	// Interval between flushes. Zero defaults to 2s.
	Interval time.Duration

	mu      sync.Mutex
	pending map[string][]func(*Credential) error
	stop    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

// Start launches the background flusher. Safe to call once.
func (d *DeferredPatcher) Start() {
	if d == nil || d.Store == nil {
		return
	}
	d.once.Do(func() {
		d.pending = make(map[string][]func(*Credential) error)
		d.stop = make(chan struct{})
		d.wg.Add(1)
		go d.loop()
	})
}

// Stop flushes remaining patches and ends the loop.
func (d *DeferredPatcher) Stop() {
	if d == nil || d.stop == nil {
		return
	}
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	d.wg.Wait()
	_ = d.Flush()
}

// Enqueue schedules a mutation for cred id. Mutations for the same id compose
// in order and are applied under one store lock during Flush.
func (d *DeferredPatcher) Enqueue(id string, mutate func(*Credential) error) {
	if d == nil || d.Store == nil || id == "" || mutate == nil {
		return
	}
	d.mu.Lock()
	if d.pending == nil {
		d.pending = make(map[string][]func(*Credential) error)
	}
	d.pending[id] = append(d.pending[id], mutate)
	d.mu.Unlock()
}

// Flush applies all pending patches in one credentials rewrite.
func (d *DeferredPatcher) Flush() error {
	if d == nil || d.Store == nil {
		return nil
	}
	d.mu.Lock()
	pending := d.pending
	d.pending = make(map[string][]func(*Credential) error)
	d.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	return d.Store.PatchMany(pending)
}

func (d *DeferredPatcher) loop() {
	defer d.wg.Done()
	interval := d.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			_ = d.Flush()
		}
	}
}

// PatchMany applies per-id mutation lists under a single lock and one save.
func (s *Store) PatchMany(pending map[string][]func(*Credential) error) error {
	if s == nil {
		return fmt.Errorf("storage: nil store")
	}
	if len(pending) == 0 {
		return nil
	}
	return s.withLock(func() error {
		doc, err := s.loadCredentials()
		if err != nil {
			return err
		}
		index := make(map[string]int, len(doc.Credentials))
		for i := range doc.Credentials {
			index[doc.Credentials[i].ID] = i
		}
		now := nowUTC()
		changed := false
		for id, muts := range pending {
			idx, ok := index[id]
			if !ok {
				continue
			}
			cur := doc.Credentials[idx]
			for _, mutate := range muts {
				if mutate == nil {
					continue
				}
				if err := mutate(&cur); err != nil {
					return err
				}
			}
			cur.ID = id
			cur.CreatedAt = doc.Credentials[idx].CreatedAt
			cur.UpdatedAt = now
			if !cur.ExpiresAt.IsZero() {
				cur.ExpiresAt = cur.ExpiresAt.UTC().Truncate(time.Second)
			}
			doc.Credentials[idx] = cur
			changed = true
		}
		if !changed {
			return nil
		}
		return s.saveCredentials(doc)
	})
}
