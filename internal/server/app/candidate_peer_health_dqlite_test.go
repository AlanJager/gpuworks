//go:build linux && dqlite

package app

import (
	"cmp"
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// A half-live peer used to stay primary on every proof because peers were
// ordered by member ID alone, so each proof paid the full hedge delay before
// reaching a live peer. A genuine failure must move that peer behind the
// healthy ones on the next proof while the quorum requirement stays put.
func TestNativeCandidateProofDeprioritizesRecentlyFailedPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var mu sync.Mutex
	firstBatch := -1
	failing := -1
	var f *candidateGrantFixture
	f = newReplicationInvalidationFixture(t, ctx, func(target int, w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != materialBatchPath {
			return false
		}
		mu.Lock()
		if firstBatch == -1 {
			firstBatch = target
		}
		reject := failing == target
		mu.Unlock()
		if reject {
			http.Error(w, "material batch source unavailable", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	publishProofLoadSources(t, ctx, f)
	order := []int{0, 1, 2}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(f.databases[a].ID, f.databases[b].ID) })
	pos := slices.Index(order, 0)
	primary, backup := order[(pos+1)%len(order)], order[(pos+2)%len(order)]
	primaryEndpoint, backupEndpoint := f.bundles[primary].info.PublicURL, f.bundles[backup].info.PublicURL
	forgetMaterialPeerFailure(primaryEndpoint)
	forgetMaterialPeerFailure(backupEndpoint)
	t.Cleanup(func() {
		forgetMaterialPeerFailure(primaryEndpoint)
		forgetMaterialPeerFailure(backupEndpoint)
	})
	prove := func() (bool, error) {
		return checkJoinedStoreMaterialReadiness(ctx, f.stores[0], f.bundles[0], f.databases[0].ID, Config{DataDir: filepath.Dir(f.receivers[0].root)})
	}

	// The primary peer accepts connections but rejects every batch with 503.
	// The proof must still complete through the hedged backup, and the failure
	// must be remembered without blaming the healthy backup.
	mu.Lock()
	failing = primary
	mu.Unlock()
	ready, err := prove()
	mu.Lock()
	first := firstBatch
	mu.Unlock()
	if !ready || err != nil {
		t.Fatalf("proof with rejecting primary failed despite live backup: ready=%t err=%v", ready, err)
	}
	if first != primary {
		t.Fatalf("first request went to peer %d, want ring-primary %d (fixture setup broken)", first, primary)
	}
	if !materialPeerFailedRecently(primaryEndpoint, time.Now().UTC()) {
		t.Fatal("proof did not remember the rejecting peer")
	}
	if materialPeerFailedRecently(backupEndpoint, time.Now().UTC()) {
		t.Fatal("healthy backup was blamed for the primary's rejection")
	}

	// With the primary healthy again, the remembered failure must keep it
	// behind the backup: the next proof's first request goes to the backup and
	// reaches quorum without paying the hedge delay for a known-bad peer.
	mu.Lock()
	failing, firstBatch = -1, -1
	mu.Unlock()
	ready, err = prove()
	mu.Lock()
	first = firstBatch
	mu.Unlock()
	if !ready || err != nil {
		t.Fatalf("proof with all peers live failed: ready=%t err=%v", ready, err)
	}
	if first != backup {
		t.Fatalf("first request went to peer %d, want recently-failed peer %d deprioritized behind %d", first, primary, backup)
	}
}
