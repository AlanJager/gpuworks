//go:build !linux || !dqlite

package app

import "testing"

// Without the replicated database a server is a single node: its App carries
// no member lifecycle and no member state.
func TestSingleNodeAppCarriesNoMemberState(t *testing.T) {
	var a App
	if a.ha != nil || a.memberState != (memberState{}) {
		t.Fatalf("a new App has member lifecycle %v and state %+v, want none", a.ha, a.memberState)
	}
}
