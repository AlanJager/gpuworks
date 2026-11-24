//go:build linux && dqlite

package app

import (
	"context"
	"sync"
)

// memberState is what an App holds as a member of a replicated control
// plane, beside what every server holds. Its zero value is a single member
// with open business and no enrollment.
type memberState struct {
	// businessHTTP admits requests to the business handler; enrollment is
	// the member's enrollment listener and material endpoint.
	businessHTTP *businessHandlerGate
	enrollment   *enrollmentServer
	// joinedClosed marks a member whose business is closed; legacyClosed one
	// that also waits for a coordinated schema migration. joinedRaft is the
	// Raft session such a member keeps while it has no store.
	joinedClosed bool
	legacyClosed bool
	joinedRaft   joinedRaftSession

	materialEligibilityMu     sync.Mutex
	materialEligibility       *materialEligibility
	materialEligibilityCancel context.CancelFunc
	materialEligibilityDone   chan struct{}
	materialEligibilityClosed bool
	// materialPending says for how long replication passes have ended
	// without completing, so that the state is reported once and not on
	// every pass.
	materialPending materialPendingNotice

	// entryCheck observes whether this process may hold the cluster entry.
	// Nil leaves a business-ready member in the candidate set.
	entryCheckMu sync.Mutex
	entryCheck   *entryCheckConfig
	// entryPreflight checks a cluster-entry address. Nil starts as the
	// current user.
	entryPreflightMu sync.Mutex
	entryPreflight   *entryPreflightConfig

	closedBusinessMu sync.Mutex
	closedBusiness   revocableBusiness

	// disaster is set when startup finds a restored data directory that
	// still requires the old member to be isolated.
	disasterMu sync.Mutex
	disaster   *DisasterRecovery
	// inferenceRole remembers the last database leadership observation so a
	// new inference request can see a role change without another lookup.
	inferenceRole inferenceRoleObservation
}
