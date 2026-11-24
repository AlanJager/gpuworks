//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestClosedMemberErrorClassOmitsWrappedErrorDetails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		class string
	}{
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"connection", driver.ErrBadConn, "bad_connection"},
		{"admission", store.ErrClusterAdmissionRejected, "admission_rejected"},
		{"identity", store.ErrClusterIdentityConflict, "identity_conflict"},
		{"maintenance", store.ErrMaintenanceNotServing, "maintenance_not_serving"},
		{"business_closed", store.ErrBusinessWritesClosed, "business_writes_closed"},
		// The class comes from the error's identity, never from its text.
		{"joined_lock", errors.Join(store.ErrBusinessWritesClosed, errors.New("database is locked")), "business_writes_closed"},
		{"lock_text_only", errors.New("database is locked"), "other"},
		{"unknown", errors.New("private-path private-key private-jwt"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("private-path private-key private-jwt: %w", tc.cause)
			if got := closedMemberErrorClass(err); got != tc.class {
				t.Fatalf("safe error classification = %q, want %q", got, tc.class)
			}
		})
	}
}
