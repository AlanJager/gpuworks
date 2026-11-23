//go:build linux && dqlite

package app

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// An election shows up as "no leader", a dropped stream, a failure frame or
// an observation that used up its bound. Each has its own class, so a
// diagnostic line says what interrupted the grant instead of "other".
func TestClosedMemberErrorClassNamesDatabaseLeaderInterruptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		class string
	}{
		{"no_leader", dqlitedriver.ErrNoAvailableLeader, "no_leader"},
		{"observation_timeout", store.ErrClusterMembershipObservationTimeout, "observation_timeout"},
		{"failure_frame", dqlitedriver.Error{Code: 3, Message: "private-path private-key"}, "interrupted_response"},
		{"dropped_stream", io.ErrUnexpectedEOF, "connection_closed"},
		{"end_of_stream", io.EOF, "connection_closed"},
		{"network", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("private-path connection reset")}, "network"},
		// A verdict is named even when an interruption is joined to it.
		{"rejected_with_no_leader", errors.Join(store.ErrClusterAdmissionRejected, dqlitedriver.ErrNoAvailableLeader), "admission_rejected"},
		{"closed_with_dropped_stream", errors.Join(store.ErrBusinessWritesClosed, io.EOF), "business_writes_closed"},
		{"maintenance_with_failure_frame", errors.Join(store.ErrMaintenanceNotServing, dqlitedriver.Error{Code: 1}), "maintenance_not_serving"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("private-path private-key private-jwt: %w", tc.cause)
			if got := closedMemberErrorClass(err); got != tc.class {
				t.Fatalf("safe error classification = %q, want %q", got, tc.class)
			}
		})
	}
}
