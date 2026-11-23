//go:build linux && dqlite

package app

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// On the real database a scheduling-pool transaction that reads before it
// owns the writer loses to a heartbeat committing on the other pool, for
// good. The first half shows that hazard on the raw scheduling pool; the
// second runs scheduling writes through the member's Store, whose pool takes
// the writer before it reads the member's admission, under the same
// heartbeats, and none may fail.
func TestNativeSchedulingWriteSurvivesHeartbeatsOnTheOtherPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	leader := f.stores[0]
	node := store.Node{UUID: "scheduling-guard-node", Name: "n"}
	if err := leader.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var beats atomic.Int64
	var heart sync.WaitGroup
	heart.Go(func() {
		for n := 0; !stop.Load(); n++ {
			if err := leader.TouchNodesAt(map[string]time.Time{node.UUID: time.Unix(int64(1_700_000_000+n), 0).UTC()}); err == nil {
				beats.Add(1)
			}
		}
	})
	defer func() { stop.Store(true); heart.Wait() }()

	// The hazard: read, then write, on the raw local pool.
	raw := f.databases[0].Scheduling
	lost := 0
	var first error
	for range 150 {
		tx, err := raw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM cluster_admissions").Scan(&n); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		_, err = tx.ExecContext(ctx, "UPDATE cluster_identities SET id = id WHERE 0")
		if err != nil {
			lost++
			if first == nil {
				first = err
			}
		}
		_ = tx.Rollback()
	}
	t.Logf("read-then-write on the raw scheduling pool: %d of 150 failed under %d heartbeats; first error: %v", lost, beats.Load(), first)
	if lost == 0 {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("the hazard did not show on this run, so the guarded half proves nothing")
		}
		t.Skip("the hazard did not show on this run; the guarded half proves nothing without it")
	}

	before := beats.Load()
	for i := range 150 {
		if _, err := leader.AdvanceSchedulingGeneration(ctx); err != nil {
			t.Fatalf("scheduling write %d through the member's Store: %v", i, err)
		}
	}
	if beats.Load() == before {
		t.Fatal("no heartbeat committed while the scheduling writes ran")
	}
	t.Logf("150 scheduling writes through the member's Store, %d heartbeats meanwhile, none failed", beats.Load()-before)
}
