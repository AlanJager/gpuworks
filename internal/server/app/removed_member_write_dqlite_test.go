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

// A member that was removed but still reaches the leader. How long it keeps
// serving rests on its confirmations and may lag; how long it keeps
// committing does not: the write reads the member's own admission in the
// transaction that writes. Three real voters, real database processes.
func TestNativeRemovedMemberCannotCommitWhileItStillServes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// renewals says what the removed member's own confirmations do after
		// the removal: answer (the refusal arrives) or stall (no answer).
		stalled bool
	}{
		{"renewals answer", false},
		{"renewals unanswered", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			f := newCandidateGrantFixture(t, ctx)
			expected := f.expected()
			removed := f.stores[1]
			key := store.SettingKey("removed-member-write-" + tc.name)
			if err := f.stores[0].SetBusinessSetting(ctx, key, `"seed"`); err != nil {
				t.Fatal(err)
			}

			// The removed member's admission, as its proof loop maintains it.
			var stall atomic.Bool
			eligibility := newMaterialEligibility(nil, func(ctx context.Context) (bool, error) {
				if stall.Load() {
					<-ctx.Done()
					return false, ctx.Err()
				}
				if _, _, err := removed.GrantClusterMembership(ctx, expected, f.keys[1]); err != nil {
					return false, err
				}
				return true, nil
			}, time.Hour)
			t.Cleanup(eligibility.Stop)
			if err := removed.InstallBusinessWriteBarrier(func(context.Context) error {
				if !eligibility.Eligible() {
					return store.ErrBusinessWritesClosed
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			eligibility.setOnInvalidated(removed.SuspendBusinessWrites)
			if ready, err := eligibility.Reprove(ctx); err != nil || !ready {
				t.Fatalf("first confirmation: ready=%t err=%v", ready, err)
			}
			if err := removed.OpenBusinessWrites(); err != nil {
				t.Fatal(err)
			}
			if err := removed.SetBusinessSetting(ctx, key, `"admitted"`); err != nil {
				t.Fatalf("admitted member could not write: %v", err)
			}

			stall.Store(tc.stalled)
			if _, err := f.stores[0].RevokeClusterAdmission(ctx, expected.ClusterID, f.keys[1]); err != nil {
				t.Fatal(err)
			}
			revokedAt := time.Now()

			// (b) Committing: the first write after the removal is refused,
			// while the member still counts itself admitted.
			err := removed.SetBusinessSetting(ctx, key, `"after removal"`)
			writeRefused := time.Since(revokedAt)
			if !errors.Is(err, store.ErrBusinessWritesClosed) || !errors.Is(err, store.ErrClusterAdmissionRejected) {
				t.Fatalf("removed member's write: %v", err)
			}
			if !eligibility.Eligible() {
				t.Fatal("the write was refused by the local admission, not by the database")
			}
			if got := f.stores[0].GetSetting(key); got != `"admitted"` {
				t.Fatalf("removed member changed the value: %s", got)
			}

			// (a) Serving: ends with the next confirmation's refusal, or, when
			// confirmations get no answer, with the window of the last one.
			if tc.stalled {
				go func() { _, _ = eligibility.Reprove(ctx) }()
				time.Sleep(2 * time.Second)
				if !eligibility.Eligible() {
					t.Fatal("unanswered renewal closed the member within 2 s")
				}
				t.Logf("write refused %v after removal; still serving 2 s after it (ends by the unconfirmed limit, %v)", writeRefused.Round(time.Millisecond), materialUnconfirmedLimit)
				return
			}
			if ready, err := eligibility.Reprove(ctx); ready || !errors.Is(err, store.ErrClusterAdmissionRejected) {
				t.Fatalf("confirmation after removal: ready=%t err=%v", ready, err)
			}
			if eligibility.Eligible() {
				t.Fatal("a refused confirmation inside the live window left the member serving")
			}
			t.Logf("write refused %v after removal; serving ended %v after it, with the next confirmation", writeRefused.Round(time.Millisecond), time.Since(revokedAt).Round(time.Millisecond))
		})
	}
}
