package app

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed means the assembled App has been finally released, rather than
// merely stopped between Runs.
var ErrClosed = errors.New("server app is closed")

// appLifetime owns the one active Run and the final close. Listener presence
// cannot express a Run that is still binding or draining, nor a Close that is
// waiting for it; these are the only additional lifecycle facts kept here.
type appLifetime struct {
	mu       sync.Mutex
	run      *activeRun
	closed   chan struct{}
	closeErr error
}

type activeRun struct {
	stop context.CancelFunc
	done chan struct{}
}

func (l *appLifetime) begin(ctx context.Context) (context.Context, func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed != nil {
		return nil, nil, ErrClosed
	}
	if l.run != nil {
		return nil, nil, errors.New("server app already running")
	}
	ctx, stop := context.WithCancel(ctx)
	run := &activeRun{stop: stop, done: make(chan struct{})}
	l.run = run
	return ctx, func() {
		stop()
		l.mu.Lock()
		l.run = nil
		close(run.done)
		l.mu.Unlock()
	}, nil
}

func (l *appLifetime) stopper() context.CancelFunc {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.run == nil {
		return nil
	}
	return l.run.stop
}

func (l *appLifetime) stop() {
	if stop := l.stopper(); stop != nil {
		stop()
	}
}

func (l *appLifetime) close(release func() error) error {
	l.mu.Lock()
	if l.closed != nil {
		done := l.closed
		l.mu.Unlock()
		<-done
		return l.closeErr
	}
	l.closed = make(chan struct{})
	run := l.run
	l.mu.Unlock()
	if run != nil {
		run.stop()
		<-run.done
	}
	l.closeErr = release()
	close(l.closed)
	return l.closeErr
}

// Close stops any active Run, waits for its final drain, and releases the
// resources New opened. It is safe to call repeatedly or concurrently. A
// stopped App can Run again; a closed App cannot.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	return a.lifetime.close(func() error {
		if a.hub != nil {
			a.hub.StopSessions()
		}
		// Release constructor-owned resources in reverse opening order. There
		// are no handlers or session/ingest writers left at this boundary.
		a.ops.close()
		if a.telemetry != nil {
			a.telemetry.exporter.Close()
		}
		if a.obs != nil {
			a.obs.Close()
		}
		a.proxy.Close()
		return a.store.Close()
	})
}
