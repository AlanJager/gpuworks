//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Low-level errors may contain material paths or credentials. Only these
// classifications cross the diagnostic boundary; the original error is retained
// by the caller for its normal fail-closed behavior.
func closedMemberErrorClass(err error) string {
	var timeout net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, driver.ErrBadConn):
		return "bad_connection"
	case errors.Is(err, store.ErrClusterAdmissionRejected):
		return "admission_rejected"
	case errors.Is(err, store.ErrClusterIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, store.ErrMaintenanceNotServing):
		return "maintenance_not_serving"
	case errors.Is(err, store.ErrBusinessWritesClosed):
		return "business_writes_closed"
	case store.ReplicatedLeaderUnavailable(err):
		return "no_leader"
	case errors.Is(err, store.ErrClusterMembershipObservationTimeout):
		return "observation_timeout"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_closed"
	case errors.As(err, &timeout):
		return "network"
	case store.ReplicatedReadInterrupted(err):
		return "interrupted_response"
	default:
		return "other"
	}
}
