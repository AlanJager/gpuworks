//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNativeMaterialBatchNegotiationKeepsEveryConfirmedGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	var mu sync.Mutex
	var counts []int
	var legacy atomic.Bool
	var f *candidateGrantFixture
	f = newReplicationInvalidationFixture(t, ctx, func(index int, w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != materialBatchPath {
			return false
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return true
		}
		var request materialBatchRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Error(err)
			return true
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		mu.Lock()
		counts = append(counts, len(request.Files))
		mu.Unlock()
		if !legacy.Load() {
			return false
		}
		if len(request.Files) > 32 || len(raw) > 4096 {
			t.Errorf("old receiver got incompatible request count%d bytes%d", len(request.Files), len(raw))
			http.Error(w, "invalid material batch entries", 400)
			return true
		}
		recorded := httptest.NewRecorder()
		enrollmentMaterialHandlerWithReceiver(f.bundles[index], f.stores[index], http.NotFoundHandler(), nil, f.receivers[index]).ServeHTTP(recorded, r)
		recorded.Header().Del(testMaterialBatchLimitHeader)
		for key, values := range recorded.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
		return true
	})
	take := func() []int { mu.Lock(); defer mu.Unlock(); got := slices.Clone(counts); counts = nil; return got }
	var items []materialBatchItem
	var allBytes int64
	for i := range 384 {
		body := []byte(fmt.Sprintf("native complete negotiated replica %d", i))
		digest := materialTestDigest(body)
		for _, target := range []int{0, 1} {
			if _, err := f.receivers[target].receive(ctx, digest, bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
		}
		items = append(items, materialBatchItem{Digest: digest, Bytes: int64(len(body))})
		allBytes += int64(len(body))
	}
	readAdmission := func() store.ClusterAdmission {
		t.Helper()
		var row store.ClusterAdmission
		if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", f.keys[0]).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	client, err := memberMaterialClient(f.bundles[0], f.bundles[1].info.PublicURL, f.keys[1])
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	for _, old := range []bool{false, true} {
		legacy.Store(old)
		before := readAdmission()
		recorder := &candidateProofRecorder{}
		proofCtx := context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
		if err := verifyCandidateRemoteMaterialItems(proofCtx, client, f.bundles[1].info.PublicURL, items, f.databases[1].ID); err != nil {
			t.Fatal(err)
		}
		got := take()
		want := []int{32, 128, 128, 96}
		if old {
			want = make([]int, 12)
			for i := range want {
				want[i] = 32
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("native complete batches=%v want%v", got, want)
		}
		after := readAdmission()
		metrics := recorder.snapshot()
		if after.MaterialGrantSequence-before.MaterialGrantSequence != int64(len(want)) || metrics.RemoteBatch.Calls != int64(len(want)) || metrics.RemoteBatch.VerifiedBytes != allBytes {
			t.Fatalf("confirmed per-request grants/bytes: delta%d metrics%+v", after.MaterialGrantSequence-before.MaterialGrantSequence, metrics.RemoteBatch)
		}
	}
	legacy.Store(false)
	original := readAdmission()
	for _, mode := range []string{"exhausted", "ignored", "failed"} {
		t.Run(mode, func(t *testing.T) {
			db := f.stores[0].DB().WithContext(ctx)
			if mode == "exhausted" {
				if err := db.Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", f.keys[0]).UpdateColumn("material_grant_sequence", int64(math.MaxInt64)).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				raise := "IGNORE"
				if mode == "failed" {
					raise = "ABORT, 'native negotiated grant failure'"
				}
				if err := db.Exec(fmt.Sprintf("CREATE TRIGGER negotiated_grant_failure BEFORE UPDATE OF material_grant_sequence ON cluster_admissions WHEN NEW.key_fingerprint='%s' BEGIN SELECT RAISE(%s); END", f.keys[0], raise)).Error; err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				if err := db.Exec("DROP TRIGGER IF EXISTS negotiated_grant_failure").Error; err != nil {
					t.Error(err)
				}
				if err := db.Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", f.keys[0]).UpdateColumn("material_grant_sequence", original.MaterialGrantSequence).Error; err != nil {
					t.Error(err)
				}
			}()
			if err := verifyCandidateRemoteMaterialItems(ctx, client, f.bundles[1].info.PublicURL, items, f.databases[1].ID); err == nil {
				t.Fatal("failed native grant authorized bytes")
			}
			if got := take(); !slices.Equal(got, []int{32}) {
				t.Fatalf("grant failure retried or reused negotiated state: %v", got)
			}
		})
	}
	// Removing the other two real voters leaves the still-running receiver
	// unable to confirm a write. No cached negotiated proof can authorize bytes.
	if err := f.databases[1].Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.databases[2].Close(); err != nil {
		t.Fatal(err)
	}
	isolatedClient, err := memberMaterialClient(f.bundles[1], f.bundles[0].info.PublicURL, f.keys[0])
	if err != nil {
		t.Fatal(err)
	}
	defer isolatedClient.CloseIdleConnections()
	if err := verifyCandidateRemoteMaterialItems(ctx, isolatedClient, f.bundles[0].info.PublicURL, items, f.databases[0].ID); err == nil {
		t.Fatal("lost native majority authorized batch")
	}
	if got := take(); !slices.Equal(got, []int{32}) {
		t.Fatalf("no-quorum request retried/upgraded: %v", got)
	}
}

func TestNativeMaterialUploadReadbackRetainsLegacy32(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	var mu sync.Mutex
	var counts []int
	f := newReplicationInvalidationFixture(t, ctx, func(index int, w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == materialBatchPath {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return true
			}
			var request materialBatchRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Error(err)
				return true
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			mu.Lock()
			counts = append(counts, len(request.Files))
			mu.Unlock()
		}
		return false
	})
	var files []MaterialFile
	local := make(map[string]string)
	for i := range 40 {
		body := []byte(fmt.Sprintf("native new upload %d", i))
		digest := materialTestDigest(body)
		path := filepath.Join(t.TempDir(), "source")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		files = append(files, MaterialFile{Class: MaterialAgentOverride, Path: path, SHA256: digest})
		local[digest] = path
	}
	_, key, expected, members, err := materialSenderMembers(ctx, f.stores[0], f.bundles[0])
	if err != nil {
		t.Fatal(err)
	}
	var target store.ClusterTransportMember
	for _, member := range members {
		if member.ID == f.databases[1].ID {
			target = member
		}
	}
	client, err := memberMaterialClient(f.bundles[0], target.EnrollmentOrigin, target.KeyFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transferred, err := replicateMaterialInventoryToPeer(ctx, f.stores[0], client, target, expected, key, files, local, func() {})
	if err != nil || !transferred {
		t.Fatalf("actual upload/readback: transferred%v err%v", transferred, err)
	}
	mu.Lock()
	got := slices.Clone(counts)
	mu.Unlock()
	if !slices.Equal(got, []int{32, 8}) {
		t.Fatalf("upload readback changed old-peer contract: %v", got)
	}
	for _, file := range files {
		info, err := os.Stat(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		found, err := f.stores[0].HasMaterialRemoteReceipt(ctx, file.SHA256, target.ID, info.Size())
		if err != nil || !found {
			t.Fatalf("actual verified receipt missing: %v", err)
		}
	}
}
