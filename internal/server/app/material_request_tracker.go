//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// materialRequestTracker accounts for admitted HTTP work until its handler
// returns. A future freeze coordinator can withdraw and wait for these local
// requests; an idle tracker alone says nothing about other members or workers.
type materialRequestTracker struct {
	mu     sync.Mutex
	active int
	closed bool
	idle   chan struct{}
	stop   context.Context
	cancel context.CancelFunc
}

func newMaterialRequestTracker() *materialRequestTracker {
	idle := make(chan struct{})
	close(idle)
	ctx, cancel := context.WithCancel(context.Background())
	return &materialRequestTracker{idle: idle, stop: ctx, cancel: cancel}
}

func (t *materialRequestTracker) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			return
		}
		stop, ok := t.begin()
		if !ok {
			http.Error(w, "material requests are closed", http.StatusServiceUnavailable)
			return
		}
		defer t.end()
		ctx, cancel := context.WithCancel(r.Context())
		stopCancel := context.AfterFunc(stop, cancel)
		defer func() { stopCancel(); cancel() }()
		if ctx.Err() == nil {
			next(w, r.WithContext(ctx))
		}
	}
}

func (t *materialRequestTracker) begin() (context.Context, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, false
	}
	if t.active == 0 {
		t.idle = make(chan struct{})
	}
	t.active++
	return t.stop, true
}

func (t *materialRequestTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	if t.active == 0 {
		close(t.idle)
	}
}

func (t *materialRequestTracker) run(ctx context.Context, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop, ok := t.begin()
	if !ok {
		return errors.New("material work is closed")
	}
	defer t.end()
	workCtx, cancel := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(stop, cancel)
	defer func() { stopCancel(); cancel() }()
	if err := workCtx.Err(); err != nil {
		return err
	}
	return work(workCtx)
}

func (t *materialRequestTracker) closeAdmissions() {
	t.mu.Lock()
	t.closed = true
	t.cancel()
	t.mu.Unlock()
}

func (t *materialRequestTracker) reopen() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active != 0 {
		return errors.New("material work is still active")
	}
	if t.closed {
		t.stop, t.cancel = context.WithCancel(context.Background())
		t.closed = false
	}
	return nil
}

func (t *materialRequestTracker) wait(ctx context.Context) error {
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// withdraw waits for already admitted handlers. A timed-out wait stays closed
// to new requests; the caller must not mistake that timeout for a drain.
func (t *materialRequestTracker) withdraw(ctx context.Context) error {
	t.closeAdmissions()
	return t.wait(ctx)
}
