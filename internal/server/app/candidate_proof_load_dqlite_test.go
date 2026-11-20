//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// All sources have real pointers, installed consumers and canonical bytes on
// all three voters. Only HTTP scheduling is observed; grants are never stubbed.
func publishProofLoadSources(t *testing.T, ctx context.Context, f *candidateGrantFixture) {
	t.Helper()
	for target := range f.databases {
		f.put(t, ctx, target, []byte("shared tiny master"))
		f.put(t, ctx, target, []byte("tiny agent override"))
		f.stores[target].NoteClusterSessionRoute(store.ClusterSessionRoute{ID: f.databases[target].ID, Secure: true})
	}
	for source := range f.databases {
		master, override := []byte("shared tiny master"), []byte("tiny agent override")
		manifest := materialManifest{Version: materialManifestVersion, ClusterID: f.bundles[source].info.ClusterID, SourceMemberID: f.databases[source].ID, SourceKey: f.keys[source], Classes: slices.Clone(requiredMaterialClasses), Files: []materialManifestFile{
			{Class: MaterialAgentOverride, Path: "dl/tiny-agent", SHA256: materialTestDigest(override), Bytes: int64(len(override))},
			{Class: MaterialMasterSecret, Path: "master.key", SHA256: materialTestDigest(master), Bytes: int64(len(master))},
		}}
		slices.SortFunc(manifest.Files, func(a, b materialManifestFile) int { return strings.Compare(string(a.Class), string(b.Class)) })
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseMaterialManifest(raw, f.expected(), manifest.SourceMemberID, manifest.SourceKey); err != nil {
			t.Fatal(err)
		}
		for target := range f.receivers {
			f.put(t, ctx, target, raw)
		}
		pointer := store.MaterialManifestRecord{SourceMemberID: int64(manifest.SourceMemberID), ClusterID: manifest.ClusterID, SourceKey: manifest.SourceKey, Digest: materialTestDigest(raw), Bytes: int64(len(raw)), RecordedAt: time.Now().UTC()}
		if err := f.stores[0].DB().WithContext(ctx).Create(&pointer).Error; err != nil {
			t.Fatal(err)
		}
		f.pointers = append(f.pointers, pointer)
	}
}

func proofLoadSequences(t *testing.T, ctx context.Context, f *candidateGrantFixture) []int64 {
	t.Helper()
	result := make([]int64, len(f.keys))
	for i, key := range f.keys {
		var row store.ClusterAdmission
		if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", key).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		result[i] = row.MaterialGrantSequence
	}
	return result
}

func TestNativeCandidateProofConcurrentRingUsesDistributedRealGrants(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var mu sync.Mutex
	first := map[int]int{}
	requests := make([]int, 3)
	var f *candidateGrantFixture
	f = newReplicationInvalidationFixture(t, ctx, func(target int, w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != materialBatchPath {
			return false
		}
		key, err := f.bundles[target].trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			t.Error(err)
			return false
		}
		source := slices.Index(f.keys, key)
		if source < 0 {
			t.Errorf("unexpected authenticated requester")
			return false
		}
		mu.Lock()
		if _, ok := first[source]; !ok {
			first[source] = target
		}
		requests[source]++
		mu.Unlock()
		return false
	})
	publishProofLoadSources(t, ctx, f)
	before := proofLoadSequences(t, ctx, f)
	ids := make([]uint64, 3)
	for i, db := range f.databases {
		ids[i] = db.ID
	}
	t.Logf("actual member IDs=%v", ids)
	order := []int{0, 1, 2}
	slices.SortFunc(order, func(a, b int) int {
		if ids[a] < ids[b] {
			return -1
		}
		if ids[a] > ids[b] {
			return 1
		}
		return 0
	})
	start := make(chan struct{})
	var work sync.WaitGroup
	metrics := make([]CandidateProofMetrics, 3)
	ready := make([]bool, 3)
	errs := make([]error, 3)
	for i := range f.databases {
		work.Add(1)
		go func() {
			defer work.Done()
			<-start
			recorder := &candidateProofRecorder{}
			proofCtx := context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
			ready[i], errs[i] = checkJoinedStoreMaterialReadiness(proofCtx, f.stores[i], f.bundles[i], ids[i], Config{DataDir: filepath.Dir(f.receivers[i].root)})
			metrics[i] = recorder.snapshot()
		}()
	}
	close(start)
	work.Wait()
	after := proofLoadSequences(t, ctx, f)
	mu.Lock()
	defer mu.Unlock()
	for pos, source := range order {
		target := order[(pos+1)%len(order)]
		completeBytes := int64(len("shared tiny master") + len("tiny agent override"))
		for index, pointer := range f.pointers {
			if index != source {
				completeBytes += pointer.Bytes
			}
		}
		t.Logf("source ID%d first peer ID%d HTTP=%d outer grants=%d verified bytes=%d committed sequence delta=%d", ids[source], ids[first[source]], requests[source], metrics[source].MembershipGrant.Calls, metrics[source].RemoteBatch.VerifiedBytes, after[source]-before[source])
		if first[source] != target {
			t.Errorf("source ID%d first authenticated peer ID%d, want ring successor ID%d", ids[source], ids[first[source]], ids[target])
		}
		if !ready[source] || errs[source] != nil {
			t.Errorf("source%d real proof ready=%t err=%v metrics=%+v", source, ready[source], errs[source], metrics[source])
		}
		if metrics[source].MembershipGrant.Calls != 2 || metrics[source].RemoteBatch.Calls != int64(requests[source]) || requests[source] != 1 {
			t.Errorf("source%d actual grants/batches=%d/%d observedHTTP=%d want2/1/1", source, metrics[source].MembershipGrant.Calls, metrics[source].RemoteBatch.Calls, requests[source])
		}
		if metrics[source].RemoteBatch.VerifiedBytes != completeBytes {
			t.Errorf("source%d remote verified bytes%d want full set%d", source, metrics[source].RemoteBatch.VerifiedBytes, completeBytes)
		}
		// Each full proof brackets all three source keys. Every HTTP request also
		// commits exactly one private grant for its authenticated requester.
		if got, want := after[source]-before[source], int64(6+requests[source]); got != want {
			t.Errorf("source%d committed sequence delta%d want%d", source, got, want)
		}
	}
}

func TestNativeCandidateProofRingSlowPrimaryUsesRealHedgeAndJoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	admitted, drained := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	requests := 0
	slow := -1
	var f *candidateGrantFixture
	f = newReplicationInvalidationFixture(t, ctx, func(target int, w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != materialBatchPath {
			return false
		}
		mu.Lock()
		requests++
		mu.Unlock()
		if target != slow {
			return false
		}
		defer close(drained)
		// Authenticate and actually commit the private grant before stalling. The
		// backup must obtain its own grant and verify the entire canonical set.
		authorizeEnrollmentMember(f.bundles[target], f.stores[target], false, func(w http.ResponseWriter, r *http.Request) {
			// Read the real batch body to EOF so net/http can observe the
			// client's cancellation while the response is held.
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error(err)
				return
			}
			close(admitted)
			<-r.Context().Done()
		})(w, r)
		return true
	})
	publishProofLoadSources(t, ctx, f)
	self := f.databases[0].ID
	order := []int{0, 1, 2}
	slices.SortFunc(order, func(a, b int) int {
		if f.databases[a].ID < f.databases[b].ID {
			return -1
		}
		if f.databases[a].ID > f.databases[b].ID {
			return 1
		}
		return 0
	})
	pos := slices.Index(order, 0)
	slow = order[(pos+1)%len(order)]
	before := proofLoadSequences(t, ctx, f)
	recorder := &candidateProofRecorder{}
	proofCtx := context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
	ready, err := checkJoinedStoreMaterialReadiness(proofCtx, f.stores[0], f.bundles[0], self, Config{DataDir: filepath.Dir(f.receivers[0].root)})
	select {
	case <-admitted:
	case <-ctx.Done():
		t.Fatal("ring primary never obtained its actual grant", ctx.Err())
	}
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("canceled slow primary did not finish its handler", ctx.Err())
	}
	mu.Lock()
	count := requests
	mu.Unlock()
	metrics := recorder.snapshot()
	completeBytes := f.pointers[1].Bytes + f.pointers[2].Bytes + int64(len("shared tiny master")+len("tiny agent override"))
	if !ready || err != nil || count != 2 || metrics.MembershipGrant.Calls != 2 || metrics.RemoteBatch.Calls != 2 || metrics.RemoteBatch.VerifiedBytes != completeBytes {
		t.Fatalf("real slow-primary hedge: ready=%t err=%v HTTP=%d metrics=%+v want fullbytes%d", ready, err, count, metrics, completeBytes)
	}
	after := proofLoadSequences(t, ctx, f)
	t.Logf("slow primary actual HTTP=%d outer grants=%d verified bytes=%d sequence deltas=%v", count, metrics.MembershipGrant.Calls, metrics.RemoteBatch.VerifiedBytes, []int64{after[0] - before[0], after[1] - before[1], after[2] - before[2]})
	for i := range after {
		want := int64(2)
		if i == 0 {
			want += 2
		}
		if got := after[i] - before[i]; got != want {
			t.Errorf("source%d actual hedge grant delta%d want%d", i, got, want)
		}
	}
}
