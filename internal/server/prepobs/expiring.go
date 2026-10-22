// Package prepobs holds transient model-preparation observations: facts that
// are only true while their producer keeps reporting them and are never
// persisted. Each producer owns its instance — the hub the agents' copy
// progress, the controller its cache evictions — and readers get copies.
package prepobs

import "time"

// expiring is a bounded map whose entries lapse ttl after their last write.
// Updating a key always succeeds in constant time; admitting a new key past
// the limit sweeps expired entries at most once per ttl and drops the write
// if the map is still full, so a reader then sees no observation. Callers
// hold their own lock.
type expiring[K comparable, V any] struct {
	ttl       time.Duration
	limit     int
	entries   map[K]expiringEntry[V]
	lastSweep time.Time
	// dropped is told about every entry a sweep removes.
	dropped func(K, V)
}

type expiringEntry[V any] struct {
	value   V
	written time.Time
}

func newExpiring[K comparable, V any](ttl time.Duration, limit int) *expiring[K, V] {
	return &expiring[K, V]{ttl: ttl, limit: limit, entries: map[K]expiringEntry[V]{}}
}

func (e *expiring[K, V]) put(key K, value V, now time.Time) bool {
	if _, exists := e.entries[key]; !exists && len(e.entries) >= e.limit {
		if now.Sub(e.lastSweep) >= e.ttl {
			e.lastSweep = now
			for k, entry := range e.entries {
				if now.Sub(entry.written) > e.ttl {
					delete(e.entries, k)
					if e.dropped != nil {
						e.dropped(k, entry.value)
					}
				}
			}
		}
		if len(e.entries) >= e.limit {
			return false
		}
	}
	e.entries[key] = expiringEntry[V]{value, now}
	return true
}

func (e *expiring[K, V]) get(key K, now time.Time) (V, bool) {
	entry, ok := e.entries[key]
	if !ok || now.Sub(entry.written) > e.ttl {
		var zero V
		return zero, false
	}
	return entry.value, true
}

func (e *expiring[K, V]) remove(key K) (V, bool) {
	entry, ok := e.entries[key]
	delete(e.entries, key)
	return entry.value, ok
}
