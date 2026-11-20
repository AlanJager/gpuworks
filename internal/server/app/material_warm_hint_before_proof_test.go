//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Actual pinned mTLS batch bytes and durable material-grant SQL contend on the
// same receiver. Only the first active proof's transaction release is controlled.
func TestWarmHintYieldsBeforeGrantBearingHTTP(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	st := f.store
	items := []materialBatchItem{f.put([]byte("warm-alpha")), f.put([]byte("warm-beta"))}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
		err := verifyRemoteMaterialBatch(ctx, f.client, f.server.URL, items, f.memberID)
		return err == nil, err
	}, materialReproofInterval)
	if ready, err := e.Reprove(ctx); !ready || err != nil {
		t.Fatalf("real initial material proof: %v %v", ready, err)
	}
	before, err := st.ClusterAdmissionForVerifiedKey(ctx, f.clusterID, f.senderKey)
	if err != nil || before.MaterialGrantSequence != 1 {
		t.Fatalf("initial durable grant: %d %v", before.MaterialGrantSequence, err)
	}
	expected, err := st.ClusterIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held, release, proofDone, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseProof := func() { releaseOnce.Do(func() { close(release) }) }
	var hold atomic.Bool
	hold.Store(true)
	const hook = "test:warm-proof-writer"
	if err := st.DB().Callback().Update().After("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "cluster_admissions" && hold.CompareAndSwap(true, false) {
			close(held)
			select {
			case <-release:
			case <-ctx.Done():
				tx.AddError(ctx.Err())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Update().Remove(hook) })
	var arrivals atomic.Int32
	hintArrived := make(chan struct{})
	f.mu.Lock()
	f.beforeServe = func(http.ResponseWriter, *http.Request) bool {
		if arrivals.Add(1) == 2 {
			close(hintArrived)
		}
		return false
	}
	f.mu.Unlock()
	proofErr := make(chan error, 1)
	go func() { defer close(proofDone); _, err := e.Reprove(ctx); proofErr <- err }()
	select {
	case <-held:
	case <-time.After(time.Second):
		releaseProof()
		t.Fatal("real proof did not hold durable grant transaction")
	}
	yielded := make(chan struct{}, 1)
	workErr := make(chan error, 1)
	var checks, repairs atomic.Int32
	// This uses the actual request guard and a fresh durable complete transport
	// grant each time. Static fixture IDs model its returned live topology only;
	// native Raft authority is outside this default test.
	guarded := guardBusinessRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), expected, 8, f.senderKey,
		func(ctx context.Context, id store.ClusterIdentity, key string) (uint64, []store.ClusterTransportMember, error) {
			revision, admissions, err := st.GrantClusterTransportAdmissions(ctx, id, key)
			if err != nil {
				return 0, nil, err
			}
			members := make([]store.ClusterTransportMember, 0, len(admissions))
			for _, a := range admissions {
				id := uint64(7)
				if a.KeyFingerprint == f.senderKey {
					id = 8
				}
				members = append(members, store.ClusterTransportMember{ID: id, KeyFingerprint: a.KeyFingerprint})
			}
			return revision, members, nil
		})
	go func() {
		defer close(workerDone)
		workErr <- flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, items,
			func(materialBatchItem) error {
				checks.Add(1)
				rec := httptest.NewRecorder()
				guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil).WithContext(ctx))
				if rec.Code != http.StatusOK {
					return errors.New("fresh per-file grant denied")
				}
				return nil
			},
			func(materialBatchItem) error {
				repairs.Add(1)
				return errors.New("verified bytes unexpectedly entered repair")
			},
			func(waitCtx context.Context) error {
				select {
				case yielded <- struct{}{}:
				default:
				}
				return (&App{memberState: memberState{materialEligibility: e}}).yieldWarmMaterialBatch(waitCtx)
			})
	}()
	t.Cleanup(func() { cancel(); releaseProof(); e.Stop(); <-proofDone; <-workerDone })
	select {
	case <-hintArrived:
		t.Error("grant-bearing warm hint reached HTTP while active proof held the durable writer, before same-group yield")
	case <-yielded:
		select {
		case <-hintArrived:
			t.Error("warm hint bypassed the captured active proof after yield entry")
		case <-time.After(30 * time.Millisecond):
		}
	case <-time.After(time.Second):
		t.Error("neither real hint nor yield boundary was reached")
	}
	releaseProof()
	if err := <-proofErr; err != nil {
		t.Fatalf("real proof bytes/grant failed: %v", err)
	}
	if err := <-workErr; err != nil {
		t.Fatalf("warm verified bytes/fresh checks failed: %v", err)
	}
	if checks.Load() != 2 || repairs.Load() != 0 {
		t.Fatalf("fresh per-file checks%d repairs%d", checks.Load(), repairs.Load())
	}
	after, err := st.ClusterAdmissionForVerifiedKey(ctx, f.clusterID, f.senderKey)
	if err != nil || after.MaterialGrantSequence != 5 {
		t.Fatalf("durable seed+proof+hint+2 per-file grants: %d %v", after.MaterialGrantSequence, err)
	}
	expectedReads := map[string]int{items[0].Digest: 3, items[1].Digest: 3}
	assertCandidateRemoteSetReads(t, f.takeRequests(), expectedReads)
}
