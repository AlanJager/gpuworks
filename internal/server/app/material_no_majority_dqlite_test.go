//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Seen on the real hosts with one member stopped: the leader closed its own
// business three times in three and a half minutes with "material manifest
// lacks a freshly verified majority", the stopped member silent and the
// other answering 503 just then. A publication that too few members answered
// is not a finding about this member: the pass stays pending and the member
// stays open. Three real voters; no peer is handed to the publication, as
// when every peer was skipped as unreachable.
func TestNativeReplicationPassWithoutAMajorityOfAnswersKeepsTheMemberOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	before, beforeErr := f.stores[0].MaterialManifestBySource(ctx, int64(f.databases[0].ID))
	err := f.stores[0].PublishMaterialManifest(ctx, f.expected(), f.databases[0].ID, f.keys[0], f.pointers[0].Digest, f.pointers[0].Bytes, nil)
	if !errors.Is(err, store.ErrMaterialManifestNoMajority) {
		t.Fatalf("publication with no verified peer: %v", err)
	}
	if materialReplicationFailureInvalidates(err) {
		t.Fatal("too few answers for a majority is taken for a finding about this member")
	}
	closed := 0
	gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
	gate.setOnInvalidated(func(admissionCloseCause) { closed++ })
	if ready, err := gate.Reprove(ctx); !ready || err != nil {
		t.Fatal(ready, err)
	}
	a := &App{memberState: memberState{materialEligibility: gate}}
	for range 3 {
		a.materialReplicationPassFailed(err)
	}
	if closed != 0 || !gate.Eligible() {
		t.Fatalf("three passes without a majority of answers closed the member: closed=%d eligible=%t", closed, gate.Eligible())
	}
	// Nothing was published on the strength of it: the pointer is as before.
	after, afterErr := f.stores[0].MaterialManifestBySource(ctx, int64(f.databases[0].ID))
	if after != before || (afterErr == nil) != (beforeErr == nil) {
		t.Fatalf("pointer changed by a refused publication: before %+v (%v), after %+v (%v)", before, beforeErr, after, afterErr)
	}
}
