//go:build linux && dqlite

package app

import (
	"errors"
	"testing"
)

func TestRecoveryKeepsSchedulingClosedUntilIsolation(t *testing.T) {
	state := BeginDisasterRecovery()
	if state.Isolated || state.Scheduling {
		t.Fatalf("recovery started open: %+v", state)
	}
	renamed := NoteClusterIDChange(state, "replacement-cluster")
	if renamed.Isolated || renamed.Scheduling {
		t.Fatalf("a new cluster id isolated the old member: %+v", renamed)
	}
	if resumed, err := ResumeRecoveryScheduling(state); err == nil || resumed.Scheduling {
		t.Fatalf("scheduling resumed without isolation: %+v %v", resumed, err)
	} else if !errors.Is(err, ErrRecoveryNotIsolated) {
		t.Fatal(err)
	}
	isolated := ConfirmMemberIsolated(state)
	if !isolated.Isolated || isolated.Scheduling {
		t.Fatalf("isolation opened scheduling: %+v", isolated)
	}
	resumed, err := ResumeRecoveryScheduling(isolated)
	if err != nil || !resumed.Scheduling || !resumed.Isolated {
		t.Fatalf("isolated recovery did not resume: %+v %v", resumed, err)
	}
}
