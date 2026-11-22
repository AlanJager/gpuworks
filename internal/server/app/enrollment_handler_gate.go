//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

type enrollmentHandlerGate struct {
	base   http.Handler
	mu     sync.Mutex
	active *enrollmentHandler
}

type enrollmentHandler struct {
	http.Handler
	inFlight int
	retired  bool
	drained  chan struct{}
}

type enrollmentHandlerDrain struct{ done <-chan struct{} }

func newEnrollmentHandlerGate(base http.Handler) *enrollmentHandlerGate {
	return &enrollmentHandlerGate{base: base}
}

// listenerObservationPath reports a route that stays with the listener's own
// handler while prepared routes are installed. The quorum observation of a
// member is read from its database process, which does not change when its
// business opens; prepared routes define neither path, so without this a
// serving member answers them with 404 and `cluster-status` observes no
// leader. Both handlers authenticate the member and are counted by the
// listener's request owner, as they are while business is closed.
func listenerObservationPath(path string) bool {
	return path == enrollment.ClusterQuorumFactsPath || path == enrollment.ClusterQuorumDiscoveryPath || path == enrollment.ClusterUpgradePath
}

func (g *enrollmentHandlerGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if listenerObservationPath(r.URL.Path) {
		g.base.ServeHTTP(w, r)
		return
	}
	g.mu.Lock()
	active := g.active
	if active != nil {
		active.inFlight++
	}
	g.mu.Unlock()
	if active != nil {
		defer g.leave(active)
		active.ServeHTTP(w, r)
		return
	}
	g.base.ServeHTTP(w, r)
}

func (g *enrollmentHandlerGate) install(handler http.Handler) {
	g.mu.Lock()
	g.retire(g.active)
	g.active = &enrollmentHandler{Handler: handler, drained: make(chan struct{})}
	g.mu.Unlock()
}

func (g *enrollmentHandlerGate) withdraw() {
	g.mu.Lock()
	g.retire(g.active)
	g.active = nil
	g.mu.Unlock()
}

// withdrawAndWait closes admission to prepared routes and returns their drain
// handle even when the caller's deadline expires.
func (g *enrollmentHandlerGate) withdrawAndWait(ctx context.Context) (*enrollmentHandlerDrain, error) {
	g.mu.Lock()
	old := g.active
	g.active = nil
	g.retire(old)
	g.mu.Unlock()
	if old == nil {
		return nil, nil
	}
	drain := &enrollmentHandlerDrain{done: old.drained}
	return drain, drain.wait(ctx)
}

func (d *enrollmentHandlerDrain) wait(ctx context.Context) error {
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retire is called with g.mu held.
func (g *enrollmentHandlerGate) retire(handler *enrollmentHandler) {
	if handler == nil || handler.retired {
		return
	}
	handler.retired = true
	if handler.inFlight == 0 {
		close(handler.drained)
	}
}

func (g *enrollmentHandlerGate) leave(handler *enrollmentHandler) {
	g.mu.Lock()
	handler.inFlight--
	if handler.retired && handler.inFlight == 0 {
		close(handler.drained)
	}
	g.mu.Unlock()
}
