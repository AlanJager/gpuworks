//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Only a verdict closes the member. A refresh that was interrupted, found the
// writer taken, ran out of its cycle or got a database failure response keeps
// the proof, whether the cycle is live or its deadline was reached: go-dqlite
// answers "no available leader" when its connect loop runs out of time, and a
// writer held through a commit stall comes back as busy just before the
// deadline. A refusal, also one such error is joined to, and an error nothing
// classifies close in either case.
func TestClosedTransportRefreshClosesOnlyOnAVerdict(t *testing.T) {
	unknown := errors.New("transport identity differs")
	for _, tc := range []struct {
		name     string
		failure  error
		deadline time.Duration // relative to now; negative is a reached deadline
		keep     bool
	}{
		{name: "reached_deadline_no_leader", failure: dqlitedriver.ErrNoAvailableLeader, deadline: -time.Millisecond, keep: true},
		{name: "reached_deadline_wrapped_no_leader", failure: errors.Join(errors.New("open connection"), dqlitedriver.ErrNoAvailableLeader), deadline: -time.Millisecond, keep: true},
		{name: "reached_deadline_dropped_stream", failure: io.ErrUnexpectedEOF, deadline: -time.Millisecond, keep: true},
		{name: "reached_deadline_observation_timeout", failure: store.ErrClusterMembershipObservationTimeout, deadline: -time.Millisecond, keep: true},
		{name: "reached_deadline_writer_busy", failure: dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"}, deadline: -time.Millisecond, keep: true},
		{name: "reached_deadline_wrapped_writer_busy", failure: errors.Join(errors.New("begin"), dqlitedriver.Error{Code: dqlitedriver.ErrBusySnapshot, Message: "database is locked"}), deadline: -time.Millisecond, keep: true},
		{name: "live_cycle_writer_busy", failure: dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"}, deadline: time.Minute, keep: true},
		{name: "reached_deadline_other_database_failure", failure: dqlitedriver.Error{Code: 19, Message: "constraint failed"}, deadline: -time.Millisecond, keep: true},
		{name: "reached_deadline_writer_busy_joined_with_rejection", failure: errors.Join(store.ErrClusterAdmissionRejected, dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"}), deadline: -time.Millisecond},
		{name: "live_cycle_no_leader", failure: dqlitedriver.ErrNoAvailableLeader, deadline: time.Minute, keep: true},
		{name: "live_cycle_dropped_stream", failure: io.ErrUnexpectedEOF, deadline: time.Minute, keep: true},
		{name: "live_cycle_interrupted_response", failure: dqlitedriver.Error{Code: 3, Message: "key"}, deadline: time.Minute, keep: true},
		{name: "live_cycle_wrapped_interrupted_response", failure: errors.Join(errors.New("begin"), dqlitedriver.Error{Code: 1, Message: "EOF"}), deadline: time.Minute, keep: true},
		{name: "live_cycle_bad_connection", failure: driver.ErrBadConn, deadline: time.Minute, keep: true},
		{name: "live_cycle_observation_timeout", failure: store.ErrClusterMembershipObservationTimeout, deadline: time.Minute, keep: true},
		{name: "live_cycle_admission_rejected", failure: store.ErrClusterAdmissionRejected, deadline: time.Minute},
		{name: "live_cycle_identity_conflict", failure: store.ErrClusterIdentityConflict, deadline: time.Minute},
		{name: "live_cycle_maintenance_not_serving", failure: store.ErrMaintenanceNotServing, deadline: time.Minute},
		{name: "live_cycle_business_writes_closed", failure: store.ErrBusinessWritesClosed, deadline: time.Minute},
		{name: "live_cycle_rejection_joined_with_interrupted_response", failure: errors.Join(store.ErrClusterAdmissionRejected, dqlitedriver.Error{Code: 3, Message: "key"}), deadline: time.Minute},
		{name: "live_cycle_rejection_joined_with_bad_connection", failure: errors.Join(store.ErrClusterAdmissionRejected, driver.ErrBadConn), deadline: time.Minute},
		{name: "reached_deadline_unknown_error", failure: unknown, deadline: -time.Millisecond},
		{name: "live_cycle_unknown_error", failure: unknown, deadline: time.Minute},
		{name: "reached_deadline_admission_rejected", failure: store.ErrClusterAdmissionRejected, deadline: -time.Millisecond},
		{name: "reached_deadline_identity_conflict", failure: store.ErrClusterIdentityConflict, deadline: -time.Millisecond},
		{name: "reached_deadline_maintenance_not_serving", failure: store.ErrMaintenanceNotServing, deadline: -time.Millisecond},
		{name: "reached_deadline_business_writes_closed", failure: store.ErrBusinessWritesClosed, deadline: -time.Millisecond},
		{name: "reached_deadline_rejection_joined_with_no_leader", failure: errors.Join(store.ErrClusterAdmissionRejected, dqlitedriver.ErrNoAvailableLeader), deadline: -time.Millisecond},
		{name: "deadline_error_joined_with_rejection", failure: errors.Join(store.ErrClusterAdmissionRejected, context.DeadlineExceeded), deadline: -time.Millisecond},
		{name: "deadline_error_joined_with_closed_writes", failure: errors.Join(store.ErrBusinessWritesClosed, context.DeadlineExceeded), deadline: time.Minute},
		{name: "deadline_error_live_cycle", failure: context.DeadlineExceeded, deadline: time.Minute, keep: true},
		{name: "reached_deadline_closed_joined_with_bad_connection", failure: errors.Join(store.ErrBusinessWritesClosed, driver.ErrBadConn), deadline: -time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
			a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("initial proof: ready=%v err=%v", ready, err)
			}
			ctx := pendingTransportDeadlineContext{Context: t.Context(), deadline: time.Now().Add(tc.deadline)}
			err := a.closedTransportAuthorizationRefresh(func(context.Context) error { return tc.failure })(ctx)
			if !errors.Is(err, tc.failure) {
				t.Fatalf("refresh error=%v, want the original %v", err, tc.failure)
			}
			if eligible := eligibility.Eligible(); eligible != tc.keep {
				t.Fatalf("refresh error=%v with deadline %v: eligible=%v, want %v", err, tc.deadline, eligible, tc.keep)
			}
		})
	}
}

// The parent's cancellation closes even when the cycle's deadline was also
// reached and the error is an interruption.
func TestClosedTransportRefreshCanceledParentClosesAfterReachedDeadline(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%v err=%v", ready, err)
	}
	parent, cancel := context.WithCancel(t.Context())
	ctx := pendingTransportDeadlineContext{Context: parent, deadline: time.Now().Add(-time.Millisecond)}
	err := a.closedTransportAuthorizationRefresh(func(context.Context) error {
		cancel()
		return dqlitedriver.ErrNoAvailableLeader
	})(ctx)
	if !errors.Is(err, dqlitedriver.ErrNoAvailableLeader) || eligibility.Eligible() {
		t.Fatalf("canceled refresh kept the proof: err=%v eligible=%v", err, eligibility.Eligible())
	}
}
