//go:build linux && dqlite

package app

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// foundingObservation is the observation of the member that created the
// cluster, for the process that did so. That process opened the database as
// a single member and has no closed listener: the listener it serves is the
// prepared one alone, which defines none of the routes other members and the
// cluster's commands ask every member (the quorum facts, the discovery of
// the members, the upgrade status, this member's part of a diagnostics
// collection). An established member serves them from its closed listener;
// this process serves the same handlers, reading through its open store,
// until its next start makes it an established member too.
//
// The routes answer with the database's member identity, which the first
// member has once its database was given the member's address and
// certificate. A member that is initialised and not yet activated has no
// such identity and no observation: nothing serves in that state, and an
// installation starts the server only after the activation.
//
// These are member-certificate routes of the enrollment listener, in front
// of its prepared routes, as an established member's are in front of its
// own: nothing that limits the business routes applies to them.
//
// The upgrade route answers the status. A step that changes the cluster is
// refused with store.ErrClusterUpgradeFoundingProcess: the migration belongs
// to an established member's session.
func (a *App) foundingObservation(r *memberRuntime) enrollmentObservation {
	if r.owner != nil || r.bundle == nil || a.store == nil {
		return nil
	}
	observed, ok := a.store.FoundingObservation(a.memberExtensionSchemas())
	if !ok {
		return nil
	}
	bundle, cfg, log := r.bundle, a.cfg, a.log
	return func(prepared http.Handler, requests *materialRequestTracker) (http.Handler, error) {
		if prepared == nil || requests == nil {
			return nil, errors.New("the observation routes need the prepared routes and their request owner")
		}
		facts, err := closedQuorumFactsHandler(bundle, observed, requests)
		if err != nil {
			return nil, err
		}
		discovery, err := closedQuorumDiscoveryHandler(bundle, observed, requests)
		if err != nil {
			return nil, err
		}
		upgrade, err := clusterUpgradeHandler(bundle, observed, requests, a.heldAgentConnections)
		if err != nil {
			return nil, err
		}
		router := chi.NewRouter()
		router.Post(enrollment.ClusterQuorumFactsPath, facts)
		router.Post(enrollment.ClusterQuorumDiscoveryPath, discovery)
		router.Post(enrollment.ClusterUpgradePath, upgrade)
		registerClusterDiagnostics(router, bundle, requests, cfg, log)
		router.NotFound(prepared.ServeHTTP)
		router.MethodNotAllowed(prepared.ServeHTTP)
		return router, nil
	}
}
