//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

func TestNativeMaterialCheckpointRequiresExactSingleCause(t *testing.T) {
	checkpoint := dqlitedriver.Error{Code: 5, Message: "checkpoint in progress"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"exact", checkpoint, true},
		{"single wrapper", fmt.Errorf("grant: %w", checkpoint), true},
		{"reachability wrapper", fmt.Errorf("%w; database fixture accepted a TCP connection", fmt.Errorf("grant: %w", checkpoint)), true},
		{"other BUSY", dqlitedriver.Error{Code: 5, Message: "database is locked"}, false},
		{"extended BUSY", dqlitedriver.Error{Code: 517, Message: checkpoint.Message}, false},
		{"LOCKED", dqlitedriver.Error{Code: 6, Message: checkpoint.Message}, false},
		{"case changed", dqlitedriver.Error{Code: 5, Message: "Checkpoint in progress"}, false},
		{"extra text", dqlitedriver.Error{Code: 5, Message: "checkpoint in progress "}, false},
		{"untyped", errors.New(checkpoint.Error()), false},
		{"one cause join", errors.Join(checkpoint), false},
		{"unavailable peer join", errors.Join(checkpoint, unavailableAuthorization()), false},
		{"integrity join", errors.Join(checkpoint, errors.New("material bytes differ")), false},
		{"unknown commit join", errors.Join(checkpoint, driver.ErrBadConn), false},
		{"canceled join", errors.Join(checkpoint, context.Canceled), false},
		{"wrapped integrity join", fmt.Errorf("peer: %w", errors.Join(checkpoint, errors.New("manifest changed"))), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := materialCheckpointContention(tc.err); got != tc.want {
				t.Fatalf("checkpoint contention=%v want=%v", got, tc.want)
			}
			// A join closes through any branch that would close on its own;
			// checkpoint, unavailable peers and a commit of unknown outcome
			// alone never close admission.
			benign := tc.want || tc.name == "one cause join" || tc.name == "unavailable peer join" || tc.name == "unknown commit join"
			if got := materialReplicationFailureInvalidates(tc.err); got == benign {
				t.Fatalf("worker invalidates=%v benign=%v", got, benign)
			}
			if peerUnreachable(tc.err) {
				t.Fatal("SQL checkpoint was misclassified as an unavailable peer")
			}
		})
	}
}
