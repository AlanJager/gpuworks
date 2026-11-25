//go:build linux && dqlite

package app

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// singleMemberObservation mounts the member observation routes on the
// listener of a server that opened the cluster's database as a single
// member. That listener is the prepared routes alone: no closed listener
// stands behind it to answer them, as one does for an established member.
// The routes need the database's member identity; a member that is
// initialised and not yet given its address and certificate has none, and
// is not served.
func (a *App) singleMemberObservation(r *memberRuntime) enrollmentObservation {
	if r.owner != nil || r.bundle == nil || a.store == nil {
		return nil
	}
	// The configured extensions, as an established member reports them: a
	// single member's runtime carries no member configuration of its own.
	observed, ok := a.store.SingleMemberObservation(MigrationSets(a.cfg.Extensions))
	if !ok {
		return nil
	}
	// This server's business is open on its own hub, which holds the agent
	// connections a stop of this member would make connect again.
	agents := func() (int, bool) {
		if a.hub == nil {
			return 0, false
		}
		return a.hub.SessionCount(), true
	}
	bundle, cfg, log := r.bundle, a.cfg, a.log
	return func(prepared http.Handler, requests *materialRequestTracker) http.Handler {
		router := chi.NewRouter()
		if err := registerMemberObservation(router, bundle, observed, requests, agents, cfg, log); err != nil {
			// A single member starts without them rather than not at all.
			log.Error("member observation routes are not served: other members and the cluster's commands cannot ask this member", "err", err)
			return prepared
		}
		router.NotFound(prepared.ServeHTTP)
		router.MethodNotAllowed(prepared.ServeHTTP)
		return router
	}
}
