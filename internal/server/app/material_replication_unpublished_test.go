//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A member that has just joined has published no manifest. Its restart check
// has nothing to compare, so the cycle replicates in full. Carried to the end
// of that first replication as a veto, the answer closed the business a fresh
// proof had opened a minute earlier. It is not a finding about material.
func TestMaterialStartupOfAnUnpublishedSourceIsNotAVeto(t *testing.T) {
	live := errors.New("independent material integrity failure")
	for _, tc := range []struct {
		name     string
		input    error
		cancel   bool
		wantVeto bool
		wantStop bool
	}{
		{name: "unpublished", input: errMaterialSourceUnpublished},
		{name: "wrapped unpublished", input: fmt.Errorf("restart check: %w", errMaterialSourceUnpublished)},
		{name: "unpublished joined with a live cause", input: errors.Join(errMaterialSourceUnpublished, live), wantVeto: true},
		{name: "unpublished joined with a root failure", input: errors.Join(errMaterialSourceUnpublished, errors.New("data root changed")), wantVeto: true},
		{name: "unpublished on a canceled cycle", input: errMaterialSourceUnpublished, cancel: true, wantVeto: true, wantStop: true},
		{name: "another missing record", input: errors.New("record not found"), wantVeto: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			invalidated := 0
			gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
			gate.setOnInvalidated(func(admissionCloseCause) { invalidated++ })
			if ready, err := gate.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			a := &App{memberState: memberState{materialEligibility: gate}}
			rejected, stop := a.materialStartupFailure(ctx, tc.input)
			if stop != tc.wantStop {
				t.Fatalf("stop = %v, want %v (rejected %v)", stop, tc.wantStop, rejected)
			}
			if tc.wantVeto {
				if invalidated != 1 || gate.Eligible() || rejected == nil {
					t.Fatalf("invalidated=%d eligible=%v rejected=%v, want the proof closed and the cause kept", invalidated, gate.Eligible(), rejected)
				}
				return
			}
			if invalidated != 0 || !gate.Eligible() || rejected != nil {
				t.Fatalf("invalidated=%d eligible=%v rejected=%v, want the proof left open and no veto", invalidated, gate.Eligible(), rejected)
			}
			// The cycle's own result decides: a clean replication reports nothing.
			if err := materialStartupResult(rejected, nil); err != nil {
				t.Fatalf("a clean first replication reported %v", err)
			}
		})
	}
}
