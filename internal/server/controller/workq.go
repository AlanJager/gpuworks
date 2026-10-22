package controller

import (
	"sync"
	"time"
)

// workq is a minimal debounced, deduplicating work queue with per-key serial
// processing. It stands in for k8s.io/client-go/util/workqueue to avoid pulling
// the full client-go dependency into a "lightweight" binary; the semantics we
// rely on (coalesce same-key enqueues, debounce bursts, process one at a time)
// are preserved.
type workq struct {
	mu       sync.Mutex
	pending  map[string]time.Time // key → earliest process time
	notify   chan struct{}
	debounce time.Duration
	closed   bool
}

func newWorkq(debounce time.Duration) *workq {
	return &workq{
		pending:  map[string]time.Time{},
		notify:   make(chan struct{}, 1),
		debounce: debounce,
	}
}

// add enqueues a key (coalescing duplicates) to run after the debounce window.
func (q *workq) add(key string) {
	q.addAfter(key, q.debounce)
}

// addAfter enqueues a key to run after delay. If the key is already pending, the
// earlier of the two times wins (so a debounce never postpones a due retry).
func (q *workq) addAfter(key string, delay time.Duration) {
	at := time.Now().Add(delay)
	q.mu.Lock()
	if !q.closed {
		if existing, ok := q.pending[key]; !ok || at.Before(existing) {
			q.pending[key] = at
		}
	}
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// next returns the next ready key, blocking until one is due or the queue closes
// (ok=false). It processes ready keys in an unspecified order.
func (q *workq) next() (string, bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return "", false
		}
		now := time.Now()
		var bestKey string
		var bestAt time.Time
		found := false
		var soonest time.Time
		for k, at := range q.pending {
			if !at.After(now) {
				if !found || at.Before(bestAt) {
					bestKey, bestAt, found = k, at, true
				}
			} else if soonest.IsZero() || at.Before(soonest) {
				soonest = at
			}
		}
		if found {
			delete(q.pending, bestKey)
			q.mu.Unlock()
			return bestKey, true
		}
		q.mu.Unlock()

		// Wait until the soonest pending key is due, or a new add arrives.
		if soonest.IsZero() {
			<-q.notify
		} else {
			t := time.NewTimer(time.Until(soonest))
			select {
			case <-q.notify:
				t.Stop()
			case <-t.C:
			}
		}
	}
}

func (q *workq) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *workq) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
