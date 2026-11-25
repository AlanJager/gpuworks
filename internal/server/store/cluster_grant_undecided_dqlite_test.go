//go:build linux && dqlite

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

// A grant error keeps a member open only when it says nothing about the
// member. A file the member cannot open or write is a finding, although its
// system call error number satisfies net.Error.
func TestGrantUndecidedIsNotFooledByFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no leader", fmt.Errorf("grant: %w", dqlitedriver.ErrNoAvailableLeader), true},
		{"dropped connection", driver.ErrBadConn, true},
		{"deadline", fmt.Errorf("grant: %w", context.DeadlineExceeded), true},
		{"leader not read in time", ErrClusterMembershipObservationTimeout, true},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, true},
		{"missing file", &fs.PathError{Op: "open", Path: "policy", Err: syscall.ENOENT}, false},
		{"disk full", &fs.PathError{Op: "write", Path: "policy", Err: syscall.ENOSPC}, false},
		{"I/O error", fmt.Errorf("persist policy: %w", &fs.PathError{Op: "fsync", Path: "policy", Err: syscall.EIO}), false},
		{"refusal", ErrClusterAdmissionRejected, false},
		{"refusal joined to a lost leader", errors.Join(dqlitedriver.ErrNoAvailableLeader, ErrClusterAdmissionRejected), false},
		{"unclassified", errors.New("something else"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GrantUndecided.Unanswered(tc.err); got != tc.want {
				t.Fatalf("undecided=%t want %t", got, tc.want)
			}
		})
	}
}
