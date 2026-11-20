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
