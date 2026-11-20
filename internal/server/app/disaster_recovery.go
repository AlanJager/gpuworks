package app

import "errors"

// ErrRecoveryNotIsolated means scheduling cannot resume until the old member
// has been isolated. A new cluster id is not isolation.
var ErrRecoveryNotIsolated = errors.New("old member is not isolated")

// DisasterRecovery is the gate in front of scheduling after a single-member restore.
type DisasterRecovery struct {
	Isolated   bool
	Scheduling bool
}

// BeginDisasterRecovery starts closed. Scheduling is off until isolation is confirmed.
func BeginDisasterRecovery() DisasterRecovery {
	return DisasterRecovery{}
}

// NoteClusterIDChange does not isolate the old member and does not open scheduling.
func NoteClusterIDChange(state DisasterRecovery, _ string) DisasterRecovery {
	return state
}

// ConfirmMemberIsolated records that the old member is stopped or partitioned.
// Scheduling stays closed.
func ConfirmMemberIsolated(state DisasterRecovery) DisasterRecovery {
	state.Isolated = true
	state.Scheduling = false
	return state
}

// ResumeRecoveryScheduling opens scheduling only after isolation.
func ResumeRecoveryScheduling(state DisasterRecovery) (DisasterRecovery, error) {
	if !state.Isolated {
		state.Scheduling = false
		return state, ErrRecoveryNotIsolated
	}
	state.Scheduling = true
	return state, nil
}
