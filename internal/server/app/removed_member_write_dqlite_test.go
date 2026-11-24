//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A member whose admission was revoked but which is still a voter and still
// reaches the leader. Serving rests on its confirmations and may lag;
// committing does not: every write transaction of a member reads the member's
// own admission, in the pool that all writes go through. Three real voters,
// real database processes.
//
// The control is a second Store on the same member's database that was never
// bound to the member's key: the same writes commit there, so it is the read,
// and nothing else about a revoked member, that refuses them.
func TestNativeRevokedMemberCannotCommitOnAnyWritePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	leader, revoked := f.stores[0], f.stores[1]
	unguarded, err := store.OpenReplicated(ctx, f.databases[1].SQL, f.databases[1].Scheduling, bus.New(), store.ReplicatedBackend())
	if err != nil {
		t.Fatal(err)
	}

	node := store.Node{UUID: "revoked-member-node", Name: "n"}
	deployment := store.Deployment{Name: "revoked-member-deployment"}
	if err := leader.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := leader.DB().Create(&deployment).Error; err != nil {
		t.Fatal(err)
	}
	setting := store.SettingKey("revoked-member-setting")
	stamp := func(n int) time.Time { return time.Unix(int64(1_700_000_000+n), 0).UTC() }
	// Each path writes the value n and reads it back from the leader.
	paths := []struct {
		name  string
		write func(*store.Store, int) error
		read  func() int
	}{
		{"business setting (was checked per method)", func(st *store.Store, n int) error {
			return st.SetBusinessSetting(ctx, setting, string(rune('0'+n)))
		}, func() int {
			if v := leader.GetSetting(setting); len(v) == 1 {
				return int(v[0] - '0')
			}
			return -1
		}},
		{"heartbeat", func(st *store.Store, n int) error {
			return st.TouchNodesAt(map[string]time.Time{node.UUID: stamp(n)})
		}, func() int {
			var got store.Node
			if err := leader.DB().First(&got, node.ID).Error; err != nil || got.LastSeenAt == nil {
				return -1
			}
			return int(got.LastSeenAt.Unix() - 1_700_000_000)
		}},
		{"deployment sleep", func(st *store.Store, n int) error {
			return st.SetDeploymentSleep(deployment.ID, n)
		}, func() int {
			var got store.Deployment
			if err := leader.DB().First(&got, deployment.ID).Error; err != nil {
				return -1
			}
			return got.DesiredSleep
		}},
		{"row created through the Store's database handle", func(st *store.Store, n int) error {
			return st.DB().Create(&store.Image{ImageRef: "revoked-member-image-" + string(rune('0'+n))}).Error
		}, func() int {
			var count int64
			leader.DB().Model(&store.Image{}).Where("image_ref LIKE ?", "revoked-member-image-%").Count(&count)
			return int(count)
		}},
		{"single statement outside a transaction", func(st *store.Store, n int) error {
			return st.DB().Exec("UPDATE nodes SET hostname = ? WHERE uuid = ?", string(rune('0'+n)), node.UUID).Error
		}, func() int {
			var got store.Node
			if err := leader.DB().First(&got, node.ID).Error; err != nil || len(got.Hostname) != 1 {
				return -1
			}
			return int(got.Hostname[0] - '0')
		}},
	}
	for _, p := range paths {
		if err := p.write(revoked, 1); err != nil {
			t.Fatalf("%s: admitted member could not write: %v", p.name, err)
		}
		if got := p.read(); got != 1 {
			t.Fatalf("%s: admitted member's write not seen on the leader: %d", p.name, got)
		}
	}

	var refusedHook atomic.Int32
	revoked.OnWriterRefused(func() { refusedHook.Add(1) })
	if _, err := leader.RevokeClusterAdmission(ctx, expected.ClusterID, f.keys[1]); err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now()
	for _, p := range paths {
		err := p.write(revoked, 2)
		if !errors.Is(err, store.ErrBusinessWritesClosed) || !errors.Is(err, store.ErrClusterAdmissionRejected) {
			t.Errorf("%s: revoked member's write: %v", p.name, err)
		}
		if got := p.read(); got != 1 {
			t.Errorf("%s: revoked member changed the value to %d", p.name, got)
		}
	}
	t.Logf("%d writes refused within %v of the revocation", len(paths), time.Since(revokedAt).Round(time.Millisecond))
	deadline := time.Now().Add(10 * time.Second)
	for refusedHook.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := refusedHook.Load(); got != int32(len(paths)) {
		t.Errorf("refusal reported %d times, want %d", got, len(paths))
	}
	// The member's own cluster writes stay possible: it must be able to ask.
	if _, _, err := revoked.GrantClusterMembership(ctx, expected, f.keys[1]); !errors.Is(err, store.ErrClusterAdmissionRejected) || errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Errorf("revoked member's own grant was not answered by the grant: %v", err)
	}
	// Control: without the member's key bound, the same database commits.
	for _, p := range paths {
		if err := p.write(unguarded, 3); err != nil {
			t.Errorf("%s: control write failed: %v", p.name, err)
		}
		if got := p.read(); got != 3 && !(p.name == "row created through the Store's database handle" && got == 2) {
			t.Errorf("%s: control write not committed: %d", p.name, got)
		}
	}
}

// How long a revoked member keeps serving: until its next confirmation is
// refused, or until a write of its own is. When its confirmations get no
// answer it keeps serving up to the unconfirmed limit, and still cannot
// commit.
func TestNativeRevokedMemberStopsServingAtItsNextAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
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
			revoked := f.stores[1]
			key := store.SettingKey("revoked-member-serving")
			var stall atomic.Bool
			eligibility := newMaterialEligibility(nil, func(ctx context.Context) (bool, error) {
				if stall.Load() {
					<-ctx.Done()
					return false, ctx.Err()
				}
				if _, _, err := revoked.GrantClusterMembership(ctx, expected, f.keys[1]); err != nil {
					return false, err
				}
				return true, nil
			}, time.Hour)
			t.Cleanup(eligibility.Stop)
			if ready, err := eligibility.Reprove(ctx); err != nil || !ready {
				t.Fatalf("first confirmation: ready=%t err=%v", ready, err)
			}
			stall.Store(tc.stalled)
			if _, err := f.stores[0].RevokeClusterAdmission(ctx, expected.ClusterID, f.keys[1]); err != nil {
				t.Fatal(err)
			}
			revokedAt := time.Now()
			if tc.stalled {
				go func() { _, _ = eligibility.Reprove(ctx) }()
				time.Sleep(2 * time.Second)
				if !eligibility.Eligible() {
					t.Fatal("an unanswered renewal closed the member within 2 s")
				}
				// A write of its own is an answer, and closes it.
				revoked.OnWriterRefused(eligibility.Invalidate)
				if err := revoked.SetBusinessSetting(ctx, key, `"x"`); !errors.Is(err, store.ErrClusterAdmissionRejected) {
					t.Fatalf("revoked member's write: %v", err)
				}
				deadline := time.Now().Add(10 * time.Second)
				for eligibility.Eligible() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if eligibility.Eligible() {
					t.Fatal("a refused write left the member serving")
				}
				return
			}
			if ready, err := eligibility.Reprove(ctx); ready || !errors.Is(err, store.ErrClusterAdmissionRejected) {
				t.Fatalf("confirmation after revocation: ready=%t err=%v", ready, err)
			}
			if eligibility.Eligible() {
				t.Fatal("a refused confirmation inside the live window left the member serving")
			}
			t.Logf("serving ended %v after the revocation, with the next confirmation", time.Since(revokedAt).Round(time.Millisecond))
			// A member that was refused comes back by itself once its row
			// is in order: the next confirmation reopens it and its writes
			// commit again. Nothing has to be restarted or reset.
			revoked.OnWriterRefused(eligibility.Invalidate)
			if err := revoked.SetBusinessSetting(ctx, key, `"refused"`); !errors.Is(err, store.ErrClusterAdmissionRejected) {
				t.Fatalf("revoked member's write: %v", err)
			}
			if err := f.stores[0].DB().Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", f.keys[1]).Update("revoked_at", nil).Error; err != nil {
				t.Fatal(err)
			}
			if ready, err := eligibility.Reprove(ctx); err != nil || !ready || !eligibility.Eligible() {
				t.Fatalf("confirmation after the admission was restored: ready=%t err=%v", ready, err)
			}
			if err := revoked.SetBusinessSetting(ctx, key, `"back"`); err != nil {
				t.Fatalf("readmitted member's write: %v", err)
			}
		})
	}
}
