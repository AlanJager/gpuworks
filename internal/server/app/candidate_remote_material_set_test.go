//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func candidateRemoteTwoSources(t *testing.T, f *candidateRemoteSetFixture) (map[uint64]store.MaterialManifestRecord, map[uint64]materialManifest, map[string]int) {
	t.Helper()
	shared := f.put([]byte("shared master material"))
	first := f.put([]byte("source one unique material"))
	second := f.put([]byte("source two unique material"))
	p1, m1 := f.source(11, []materialBatchItem{shared, first})
	p2, m2 := f.source(12, []materialBatchItem{shared, second})
	return map[uint64]store.MaterialManifestRecord{11: p1, 12: p2}, map[uint64]materialManifest{11: m1, 12: m2}, map[string]int{shared.Digest: 1, first.Digest: 1, second.Digest: 1, p1.Digest: 1, p2.Digest: 1}
}

func TestCandidateRemoteMaterialSetsShareDigestsAndReadEveryPointer(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	pointers, manifests, expected := candidateRemoteTwoSources(t, f)
	// Separate single-source calls reproduce the former sequential request
	// pattern; this is a quantitative baseline, not an old native proof.
	for _, id := range []uint64{11, 12} {
		if err := verifyCandidateRemoteMaterialSets(t.Context(), f.client, f.server.URL, map[uint64]store.MaterialManifestRecord{id: pointers[id]}, map[uint64]materialManifest{id: manifests[id]}, 7); err != nil {
			t.Fatal(err)
		}
	}
	baseline := f.takeRequests()
	if len(baseline) != 2 {
		t.Fatalf("single-source requests=%d", len(baseline))
	}
	baselineCounts := make(map[string]int)
	for k, v := range expected {
		baselineCounts[k] = v
	}
	baselineCounts[manifests[11].Files[0].SHA256] = 2
	assertCandidateRemoteSetReads(t, baseline, baselineCounts)
	for range 2 {
		if err := verifyCandidateRemoteMaterialSets(t.Context(), f.client, f.server.URL, pointers, manifests, 7); err != nil {
			t.Fatal(err)
		}
		requests := f.takeRequests()
		if len(requests) != 1 {
			t.Fatalf("merged requests=%d", len(requests))
		}
		assertCandidateRemoteSetReads(t, requests, expected)
	}
	other := newCandidateRemoteSetFixture(t, 8)
	for digest := range expected {
		raw, err := os.ReadFile(filepath.Join(f.root, digest[:2], digest))
		if err != nil {
			t.Fatal(err)
		}
		other.put(raw)
	}
	if err := verifyCandidateRemoteMaterialSets(t.Context(), other.client, other.server.URL, pointers, manifests, 8); err != nil {
		t.Fatal(err)
	}
	assertCandidateRemoteSetReads(t, other.takeRequests(), expected)
}

func TestCandidateRemoteMaterialSetsConflictingSizesBeforeRequest(t *testing.T) {
	for _, shape := range []string{"shared-file", "pointer-file", "pointer-pointer"} {
		t.Run(shape, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			p, m, _ := candidateRemoteTwoSources(t, f)
			switch shape {
			case "shared-file":
				mm := m[12]
				mm.Files[0].Bytes++
				m[12] = mm
			case "pointer-file":
				mm := m[12]
				mm.Files[0].SHA256 = p[11].Digest
				mm.Files[0].Bytes = p[11].Bytes + 1
				m[12] = mm
			case "pointer-pointer":
				pp := p[12]
				pp.Digest = p[11].Digest
				pp.Bytes = p[11].Bytes + 1
				p[12] = pp
			}
			if err := verifyCandidateRemoteMaterialSets(t.Context(), f.client, f.server.URL, p, m, 7); err == nil {
				t.Fatal("contradictory source accepted")
			}
			if requests := f.takeRequests(); len(requests) != 0 {
				t.Fatalf("contradiction sent requests: %v", requests)
			}
		})
	}
}

func TestCandidateRemoteMaterialSetsSecondSourceFailures(t *testing.T) {
	for _, shape := range []string{"missing-file", "corrupt-file", "missing-pointer", "corrupt-pointer", "wrong-member"} {
		t.Run(shape, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			p, m, _ := candidateRemoteTwoSources(t, f)
			item := materialBatchItem{Digest: m[12].Files[1].SHA256, Bytes: m[12].Files[1].Bytes}
			if shape == "missing-pointer" || shape == "corrupt-pointer" {
				item = materialBatchItem{Digest: p[12].Digest, Bytes: p[12].Bytes}
			}
			name := filepath.Join(f.root, item.Digest[:2], item.Digest)
			switch shape {
			case "missing-file", "missing-pointer":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "corrupt-file", "corrupt-pointer":
				if err := os.WriteFile(name, bytes.Repeat([]byte("x"), int(item.Bytes)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			member := uint64(7)
			if shape == "wrong-member" {
				member = 8
			}
			if err := verifyCandidateRemoteMaterialSets(t.Context(), f.client, f.server.URL, p, m, member); err == nil {
				t.Fatal("invalid second-source proof accepted")
			}
			if requests := f.takeRequests(); len(requests) != 1 {
				t.Fatalf("actual receiver was not requested: %v", requests)
			}
		})
	}
}

func TestCandidateRemoteMaterialSetsPreserveBatchAndLargeGET(t *testing.T) {
	for _, shape := range []string{"count", "aggregate-bytes", "large-shared"} {
		t.Run(shape, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			var files []materialBatchItem
			switch shape {
			case "count":
				for i := range maxMaterialBatchCount + 1 {
					files = append(files, f.put([]byte(fmt.Sprintf("canonical object %d", i))))
				}
			case "aggregate-bytes":
				files = []materialBatchItem{f.put(bytes.Repeat([]byte("a"), 4<<20)), f.put(bytes.Repeat([]byte("b"), 4<<20))}
			case "large-shared":
				files = []materialBatchItem{f.put(bytes.Repeat([]byte("L"), int(maxMaterialBatchBytes)+1))}
			}
			p1, m1 := f.source(11, files)
			p := map[uint64]store.MaterialManifestRecord{11: p1}
			m := map[uint64]materialManifest{11: m1}
			if shape == "large-shared" {
				p2, m2 := f.source(12, files)
				p[12] = p2
				m[12] = m2
			}
			if err := verifyCandidateRemoteMaterialSets(t.Context(), f.client, f.server.URL, p, m, 7); err != nil {
				t.Fatal(err)
			}
			expected := make(map[string]int)
			for _, pointer := range p {
				expected[pointer.Digest] = 1
			}
			for _, file := range files {
				expected[file.Digest] = 1
			}
			requests := f.takeRequests()
			assertCandidateRemoteSetReads(t, requests, expected)
			want := 2
			if shape == "large-shared" {
				want = 3
			}
			if len(requests) != want {
				t.Fatalf("requests=%d want%d: %v", len(requests), want, requests)
			}
			for _, request := range requests {
				if len(request) == 1 && request[0].Bytes == 0 {
					if shape != "large-shared" || request[0].Digest != files[0].Digest {
						t.Fatal("unexpected GET")
					}
					continue
				}
				if _, err := materialBatchLength(request); err != nil {
					t.Fatalf("batch bounds violated: %v", err)
				}
			}
		})
	}
}

func TestCandidateRemoteMaterialSetsCanceledBodyStopsFurtherBatches(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	files := make([]materialBatchItem, 0, maxMaterialBatchCount+1)
	for i := range maxMaterialBatchCount + 1 {
		files = append(files, f.put([]byte(fmt.Sprintf("cancel material %d", i))))
	}
	p, m := f.source(11, files)
	entered, handlerDone := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
		defer close(handlerDone)
		var request materialBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return true
		}
		// Produce genuine receiver frames, then hold their final byte so the
		// client has an actual incomplete TLS body when canceled.
		raw, err := json.Marshal(request)
		if err != nil {
			t.Error(err)
			return true
		}
		recorded := httptest.NewRecorder()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		f.handler.ServeHTTP(recorded, r)
		if recorded.Code != http.StatusOK {
			t.Errorf("actual receiver status=%d", recorded.Code)
			return true
		}
		for key, values := range recorded.Header() {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes()[:recorded.Body.Len()-1])
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		return true
	}
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- verifyCandidateRemoteMaterialSets(ctx, f.client, f.server.URL, map[uint64]store.MaterialManifestRecord{11: p}, map[uint64]materialManifest{11: m}, 7)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual body not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled partial body accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("verification did not return after cancel")
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("actual HTTP handler not canceled")
	}
	requests := f.takeRequests()
	if len(requests) != 1 || len(requests[0]) != materialBatchLegacyCount {
		t.Fatalf("canceled proof sent further batches: %v", requests)
	}
	// The original caller's context remains canceled; a fresh proof must do
	// its own reads rather than reuse the incomplete attempt.
	f.mu.Lock()
	f.beforeServe = nil
	f.mu.Unlock()
	if err := verifyCandidateRemoteMaterialSets(t.Context(), f.client, f.server.URL, map[uint64]store.MaterialManifestRecord{11: p}, map[uint64]materialManifest{11: m}, 7); err != nil {
		t.Fatal(err)
	}
	if requests := f.takeRequests(); len(requests) != 2 {
		t.Fatalf("fresh proof requests=%d", len(requests))
	}
}
