package app

import (
	"context"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
)

// haRuntime is what membership of a replicated control plane adds to one
// server's lifecycle. A single-node server has none: every call site below
// then does exactly what it did before there were members.
//
// A member is in one of two states. While its business is closed it has no
// business store: New returns an App that only carries the runtime, and
// runClosed serves it. Once business is open the App is assembled as on a
// single node and the runtime takes part at the remaining hook points.
type haRuntime interface {
	// runClosed serves a member whose business is closed until ctx ends or
	// serving fails. It reports false, without doing anything, for a member
	// whose business is open: Run then serves business.
	runClosed(ctx context.Context, a *App) (handled bool, err error)
	// generation reports a business App that is one generation of a closed
	// member rather than a server of its own: it borrows the member's store
	// and listeners, so assembly leaves the stored state as the cluster has
	// it and Close leaves the store open.
	generation() bool
	// assemble adds the member's part to a business App being assembled, once
	// its token signer and catalog package store exist: it checks the store
	// against the member's enrollment and prepares the enrollment endpoint.
	assemble(a *App, signer *auth.TokenSigner, catalog *packages.Store) error
	// handler puts the member's business admission in front of the business
	// handler. A closed or ineligible member answers 503 there.
	handler(next http.Handler) http.Handler
	// start brings up what a serving member runs beside business, after the
	// business listeners are bound and before any business loop: the
	// enrollment listener, transport authorization and material replication.
	// A later failure that must end Run is sent on failed. stop is called
	// once, after Run has drained business.
	start(ctx context.Context, a *App, failed chan<- error) (stop func(), err error)
	// runController runs the controller only while this member is the
	// local scheduling leader, until ctx ends.
	runController(ctx context.Context, a *App, run func(context.Context))
	// beforeClose withdraws what still uses the store. While it fails the
	// App keeps its store and its runtime lease, and Close reports the error.
	beforeClose(a *App) error
	// afterClose releases what outlives the store, such as a member's Raft
	// session.
	afterClose(a *App) error
}

// memberHandler is the business handler behind the member's admission.
func (a *App) memberHandler(next http.Handler) http.Handler {
	if a.ha == nil {
		return next
	}
	return a.ha.handler(next)
}

// runClosedMember serves a member whose business is closed.
func (a *App) runClosedMember(ctx context.Context) (bool, error) {
	if a.ha == nil {
		return false, nil
	}
	return a.ha.runClosed(ctx, a)
}

// startMember starts the member's own listeners and control loops.
func (a *App) startMember(ctx context.Context, failed chan<- error) (func(), error) {
	if a.ha == nil {
		return func() {}, nil
	}
	stop, err := a.ha.start(ctx, a, failed)
	if err != nil {
		return nil, err
	}
	if stop == nil {
		stop = func() {}
	}
	return stop, nil
}

// runController reconciles deployments into instances. Among members only
// the local scheduling leader does.
func (a *App) runController(ctx context.Context) {
	if a.ha == nil {
		a.ctrl.Run(ctx)
		return
	}
	a.ha.runController(ctx, a, a.ctrl.Run)
}
