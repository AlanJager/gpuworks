//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"syscall"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

func TestMaterialReplicationFailurePreservesIntegrityCauses(t *testing.T) {
	unavailable := &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: syscall.ECONNREFUSED}
	integrity := errors.New("material digest differs")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no failure", nil, false},
		{"unreachable", unavailable, false},
		{"wrapped unreachable", fmt.Errorf("peer: %w", unavailable), false},
		{"checkpoint text", errors.New("checkpoint in progress"), true},
		{"HTTP checkpoint text", errors.New("HTTP 503: checkpoint in progress"), true},
		{"metadata", errors.New("material batch metadata differs"), true},
		{"integrity", integrity, true},
		{"admission", errors.New("cluster admission was revoked"), true},
		{"unknown commit", driver.ErrBadConn, true},
		{"caller cancellation", context.Canceled, true},
		{"pass ran out of time", context.DeadlineExceeded, false},
		{"wrapped pass ran out of time", fmt.Errorf("index material inventory locally: %w", context.DeadlineExceeded), false},
		{"no database leader", fmt.Errorf("grant: %w", dqlitedriver.ErrNoAvailableLeader), false},
		{"joined timeout integrity", errors.Join(context.DeadlineExceeded, integrity), true},
		{"joined timeout unknown commit", errors.Join(context.DeadlineExceeded, driver.ErrBadConn), true},
		{"joined timeout cancellation", errors.Join(context.DeadlineExceeded, context.Canceled), true},
		{"joined timeout unreachable", errors.Join(context.DeadlineExceeded, unavailable), false},
		{"joined timeout no leader", errors.Join(context.DeadlineExceeded, dqlitedriver.ErrNoAvailableLeader), false},
		{"joined unreachable integrity", errors.Join(unavailable, integrity), true},
		{"wrapped joined integrity", fmt.Errorf("peer: %w", errors.Join(unavailable, integrity)), true},
		{"joined unreachable unknown commit", errors.Join(unavailable, driver.ErrBadConn), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := materialReplicationFailureInvalidates(tc.err); got != tc.want {
				t.Fatalf("invalidates=%v want=%v", got, tc.want)
			}
		})
	}
}
