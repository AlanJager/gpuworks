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

// close stops the active Run, then calls withdraw and release. A failed
// withdraw is not final: nothing has been released, so the lifetime stays
// open and a later close tries again. Once release has run the lifetime is
// closed for good, whatever release returned.
func (l *appLifetime) close(withdraw, release func() error) error {
	l.mu.Lock()
	if l.closed != nil {
		done := l.closed
		l.mu.Unlock()
		<-done
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.closeErr
	}
	closing := make(chan struct{})
	l.closed = closing
	run := l.run
	l.mu.Unlock()
	if run != nil {
		run.stop()
		<-run.done
	}
	if err := withdraw(); err != nil {
		l.mu.Lock()
		l.closeErr = err
		l.closed = nil
		l.mu.Unlock()
		close(closing)
		return err
	}
	err := release()
	l.mu.Lock()
	l.closeErr = err
	l.mu.Unlock()
	close(closing)
	return err
}

// Close stops any active Run, waits for its final drain, and releases the
// resources New opened. It is safe to call repeatedly or concurrently. A
// stopped App can Run again; a closed App cannot. A member that cannot yet
// withdraw from its store fails Close without releasing anything, and Close
// can be called again.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	return a.lifetime.close(func() error {
		// A member first withdraws what still uses its store; while that
		// fails the store and its runtime lease stay with this App.
		if a.ha == nil {
			return nil
		}
		return a.ha.beforeClose(a)
	}, a.release)
}

// release frees what New opened, in reverse opening order. There are no
// handlers or session/ingest writers left at this boundary.
func (a *App) release() error {
	if a.hub != nil {
		a.hub.StopSessions()
	}
	a.ops.close()
	if a.telemetry != nil {
		a.telemetry.exporter.Close()
	}
	if a.obs != nil {
		a.obs.Close()
	}
	if a.proxy != nil {
		a.proxy.Close()
	}
	var err error
	// A member's business generation borrows the member's store.
	if a.store != nil && (a.ha == nil || !a.ha.generation()) {
		err = a.store.Close()
	}
	if a.ha != nil {
		err = errors.Join(err, a.ha.afterClose(a))
	}
	return err
}
