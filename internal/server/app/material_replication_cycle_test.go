//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialPeriodicCycleCanceledBeforeWorkKeepsAuthoritySeparate(t *testing.T) {
	for _, mode := range []string{"periodic", "force", "new-members", "no-installed-digest"} {
		t.Run(mode, func(t *testing.T) {
			var proofs, invalidated atomic.Int32
			eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) {
				proofs.Add(1)
				return true, nil
			}, materialReproofInterval)
			eligibility.setOnInvalidated(func(admissionCloseCause) { invalidated.Add(1) })
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			deadline, epoch := eligibility.deadline, eligibility.epoch
			a := &App{memberState: memberState{materialEligibility: eligibility}}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			digest := "installed"
			if mode == "no-installed-digest" {
				digest = ""
			}
			// No Store or enrollment exists: cancellation must return before
			// either the installed checker or the original repair can touch I/O.
			err := a.replicateMaterialCycle(ctx, nil, 1, "self", store.ClusterIdentity{}, nil, digest, mode != "new-members", mode == "force")
			if !errors.Is(err, context.Canceled) || proofs.Load() != 1 || invalidated.Load() != 0 || !eligibility.Eligible() || eligibility.deadline != deadline || eligibility.epoch != epoch {
				t.Fatalf("canceled planning changed authority or started work: err=%v proofs=%d invalidated=%d", err, proofs.Load(), invalidated.Load())
			}
		})
	}
}
