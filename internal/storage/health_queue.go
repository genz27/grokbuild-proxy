package storage

import (
	"fmt"
	"time"
)

// HealthQueue adapts DeferredPatcher for lb health updates and usage touches.
// It also satisfies PatchCredential so it can be used as lb healthStore.
type HealthQueue struct {
	Store *Store
	*DeferredPatcher
}

// NewHealthQueue builds a queue bound to store.
func NewHealthQueue(store *Store, interval time.Duration) *HealthQueue {
	d := &DeferredPatcher{Store: store, Interval: interval}
	return &HealthQueue{Store: store, DeferredPatcher: d}
}

// PatchCredential applies immediately (token rotates must be durable now).
func (q *HealthQueue) PatchCredential(id string, mutate func(*Credential) error) (Credential, error) {
	if q == nil || q.Store == nil {
		return Credential{}, fmt.Errorf("storage: nil store")
	}
	return q.Store.PatchCredential(id, mutate)
}

// EnqueueHealth implements the deferred health store used by lb.Selector.
func (q *HealthQueue) EnqueueHealth(id string, mutate func(*Credential) error) {
	if q == nil || q.DeferredPatcher == nil {
		return
	}
	q.Enqueue(id, mutate)
}

// EnqueueLastUsed schedules a last_used_at update for later flush.
func (q *HealthQueue) EnqueueLastUsed(id string, at time.Time) {
	if q == nil || q.DeferredPatcher == nil || id == "" {
		return
	}
	ts := at.UTC().Truncate(time.Second)
	q.Enqueue(id, func(c *Credential) error {
		c.LastUsedAt = &ts
		return nil
	})
}
