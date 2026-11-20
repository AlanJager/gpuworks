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
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const testMaterialBatchLimitHeader = "X-Gpuworks-Material-Batch-Limit"

func negotiatedMaterialItems(t *testing.T, f *candidateRemoteSetFixture, count int) []materialBatchItem {
	t.Helper()
	files := make([]materialBatchItem, 0, count)
	for i := range count {
		files = append(files, f.put([]byte(fmt.Sprintf("distinct negotiated material %d", i))))
	}
	return files
}

func TestMaterialBatchNegotiationReadsEveryByteWithFewerRequests(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	files := negotiatedMaterialItems(t, f, 383)
	pointer, manifest := f.source(11, files)
	expected := map[string]int{pointer.Digest: 1}
	var allBytes int64 = pointer.Bytes
	for _, file := range files {
		expected[file.Digest] = 1
		allBytes += file.Bytes
	}
	for range 2 {
		recorder := &candidateProofRecorder{}
		ctx := context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder)
		if err := verifyCandidateRemoteMaterialSets(ctx, f.client, f.server.URL, map[uint64]store.MaterialManifestRecord{11: pointer}, map[uint64]materialManifest{11: manifest}, 7); err != nil {
			t.Fatal(err)
		}
		requests := f.takeRequests()
		assertCandidateRemoteSetReads(t, requests, expected)
		if len(requests) != 4 {
			t.Fatalf("complete proof requests=%d want4 (legacy32 requires12)", len(requests))
		}
		for i, want := range []int{32, 128, 128, 96} {
			if len(requests[i]) != want {
				t.Fatalf("request%d count=%d want%d", i, len(requests[i]), want)
			}
		}
		metrics := recorder.snapshot()
		if metrics.RemoteBatch.Calls != 4 || metrics.RemoteBatch.VerifiedBytes != allBytes {
			t.Fatalf("complete proof accounting: %+v want bytes%d", metrics.RemoteBatch, allBytes)
		}
	}
}

func TestMaterialBatchNegotiationRequiresCompleteSuccessfulResponse(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	files := negotiatedMaterialItems(t, f, 160)
	pointer, manifest := f.source(11, files)
	expected := map[string]int{pointer.Digest: 1}
	for _, file := range files {
		expected[file.Digest] = 1
	}
	for _, mode := range []string{"legacy32", "duplicate-header", "combined-header", "unknown-header", "wrong-member", "wrong-content-length", "corrupt", "truncated", "cancel", "service-unavailable", "bad-request"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.mu.Lock()
			f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				if mode == "legacy32" {
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
					if len(request.Files) > 32 || len(raw) > 4096 {
						t.Errorf("old receiver got nonlegacy request: count%d bytes%d", len(request.Files), len(raw))
						http.Error(w, "invalid material batch entries", 400)
						return true
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
				}
				recorded := httptest.NewRecorder()
				f.handler.ServeHTTP(recorded, r)
				if recorded.Code != 200 {
					t.Errorf("real authenticated handler status%d", recorded.Code)
				}
				body := bytes.Clone(recorded.Body.Bytes())
				switch mode {
				case "legacy32":
					recorded.Header().Del(testMaterialBatchLimitHeader)
				case "duplicate-header":
					recorded.Header()[testMaterialBatchLimitHeader] = []string{"128", "128"}
				case "combined-header":
					recorded.Header().Set(testMaterialBatchLimitHeader, "128, 128")
				case "unknown-header":
					recorded.Header().Set(testMaterialBatchLimitHeader, "999999")
				case "wrong-member":
					recorded.Header().Set("X-Gpuworks-Material-Member", "8")
				case "wrong-content-length":
					recorded.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
				case "corrupt":
					body[len(body)-1] ^= 1
				case "truncated":
					body = body[:len(body)-1]
				case "cancel":
					cancel()
				case "service-unavailable", "bad-request":
					w.Header().Set(testMaterialBatchLimitHeader, "128")
					status := http.StatusServiceUnavailable
					if mode == "bad-request" {
						status = 400
					}
					http.Error(w, "rejected", status)
					return true
				}
				for key, values := range recorded.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorded.Code)
				_, _ = w.Write(body)
				return true
			}
			f.mu.Unlock()
			err := verifyCandidateRemoteMaterialSets(ctx, f.client, f.server.URL, map[uint64]store.MaterialManifestRecord{11: pointer}, map[uint64]materialManifest{11: manifest}, 7)
			requests := f.takeRequests()
			successful := mode == "legacy32" || mode == "duplicate-header" || mode == "combined-header" || mode == "unknown-header"
			if successful {
				if err != nil {
					t.Fatal(err)
				}
				assertCandidateRemoteSetReads(t, requests, expected)
				if len(requests) != 6 {
					t.Fatalf("unnegotiated peer requests=%d want6", len(requests))
				}
				for _, request := range requests {
					if len(request) > 32 {
						t.Fatal("unconfirmed hint increased batch count")
					}
				}
			} else if err == nil || len(requests) != 1 {
				t.Fatalf("failed response retried/upgraded: err=%v requests%d", err, len(requests))
			}
		})
	}
}

func TestMaterialBatchNegotiatedHandlerKeepsFiniteBounds(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	files := negotiatedMaterialItems(t, f, 129)
	// Seven-digit size is the maximum for any individual batch entry. This
	// deterministic serialized bound is conservative even before aggregate checks.
	worst := make([]materialBatchItem, 128)
	for i := range worst {
		worst[i] = materialBatchItem{Digest: fmt.Sprintf("%064x", i), Bytes: 8 << 20}
	}
	raw, err := json.Marshal(materialBatchRequest{Files: worst})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 12043 || len(raw) > 16<<10 {
		t.Fatalf("maximum JSON bytes%d", len(raw))
	}
	send := func(t *testing.T, items []materialBatchItem, extra int, want int) {
		t.Helper()
		raw, err := json.Marshal(materialBatchRequest{Files: items})
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, bytes.Repeat([]byte(" "), extra)...)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+materialBatchPath, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := f.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("production batch handler status%d want%d", response.StatusCode, want)
		}
		if want == 200 {
			if got := response.Header.Values(testMaterialBatchLimitHeader); len(got) != 1 || got[0] != "128" {
				t.Fatalf("successful capacity declaration: %v", got)
			}
			if err := verifyMaterialBatchBody(t.Context(), response.Body, items); err != nil {
				t.Fatal(err)
			}
		} else {
			_, _ = io.Copy(io.Discard, response.Body)
		}
	}
	t.Run("complete128", func(t *testing.T) { send(t, files[:128], 0, 200) })
	t.Run("count129", func(t *testing.T) { send(t, files, 0, 400) })
	t.Run("duplicate", func(t *testing.T) { send(t, []materialBatchItem{files[0], files[0]}, 0, 400) })
	t.Run("metadata16KiB", func(t *testing.T) { send(t, files[:1], 16<<10, 400) })
	t.Run("individual8MiB", func(t *testing.T) {
		send(t, []materialBatchItem{{Digest: files[0].Digest, Bytes: (8 << 20) + 1}}, 0, 400)
	})
	t.Run("aggregate8MiB", func(t *testing.T) {
		send(t, []materialBatchItem{{Digest: files[0].Digest, Bytes: 4 << 20}, {Digest: files[1].Digest, Bytes: (4 << 20) + 1}}, 0, 400)
	})
}
