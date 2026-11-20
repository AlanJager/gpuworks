//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// closedBusinessGeneration borrows the closed App's Store and listeners. A
// timed-out shutdown retains this owner so Close can wait again before Store
// or observability resources are released.
type closedBusinessGeneration struct {
	owner     *App
	business  *App
	phaseMu   sync.Mutex
	stopping  bool
	stopOnce  sync.Once
	setupDone chan struct{}
	done      chan struct{}
	stopErr   error
	cancel    context.CancelFunc
	started   bool
	workers   *businessWorkers
	// stopSessions closes the member-session route this generation opened.
	stopSessions func() error
	public       *businessHandlerDrain
	enrollment   *enrollmentHandlerDrain
}

func (g *closedBusinessGeneration) Stopping() bool {
	g.phaseMu.Lock()
	defer g.phaseMu.Unlock()
	return g.stopping
}

func (g *closedBusinessGeneration) BeginStop() {
	g.stopOnce.Do(func() {
		g.owner.store.SuspendBusinessWrites()
		g.phaseMu.Lock()
		g.stopping = true
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		g.public, _ = g.owner.businessHTTP.withdrawBusinessHandler(canceled)
		g.enrollment, _ = g.owner.enrollment.withdrawPreparedHandlerAndWait(canceled)
		if g.cancel != nil {
			g.cancel()
		}
		g.phaseMu.Unlock()
		go g.drain()
	})
}

func (g *closedBusinessGeneration) Wait(ctx context.Context) error {
	select {
	case <-g.done:
		return g.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *closedBusinessGeneration) drain() {
	defer close(g.done)
	<-g.setupDone
	a := g.business
	if g.cancel != nil {
		g.cancel()
	}
	if g.started {
		a.proxy.StopHolding()
		a.hub.StopSessions()
	}
	if g.stopSessions != nil {
		g.stopErr = errors.Join(g.stopErr, g.stopSessions())
	}
	// Requests admitted to this generation finish through its handlers;
	// its workers may still complete some of them.
	if g.public != nil {
		g.stopErr = errors.Join(g.stopErr, g.public.wait(context.Background()))
	}
	if g.workers != nil {
		// The owner must keep observability open until accounting and ingest
		// have actually finished, even after their grace periods expire.
		g.workers.stop()
	}
	if g.enrollment != nil {
		g.stopErr = errors.Join(g.stopErr, g.enrollment.wait(context.Background()))
	}
	g.stopErr = errors.Join(g.stopErr, g.owner.store.CloseBusinessWrites(context.Background()))
	g.owner.store.SetSecretbox(nil)
	g.owner.store.ReleaseNodeAdmission()
	// The generation borrowed the member's store: Close releases only what
	// its assembly opened.
	g.stopErr = errors.Join(g.stopErr, a.Close())
}

// withinGeneration ends a request admitted by one business generation when
// that generation ends. The member's listeners outlive every generation, so
// without this a long-lived response would keep a withdrawn generation, and
// the store admission it holds, from ever draining.
func withinGeneration(generation context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(generation, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
