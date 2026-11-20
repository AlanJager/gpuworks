//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCandidateVerifiedProgressContinuedBatchesAvoidBackup(t *testing.T) {
	primary := newCandidateRemoteSetFixture(t, 7)
	files := negotiatedMaterialItems(t, primary, 383)
	pointer, manifest := primary.source(11, files)
	pointers := map[uint64]store.MaterialManifestRecord{11: pointer}
	manifests := map[uint64]materialManifest{11: manifest}
	expected := map[string]int{pointer.Digest: 1}
	for _, file := range files {
		expected[file.Digest] = 1
	}
	primary.beforeServe = func(_ http.ResponseWriter, r *http.Request) bool {
		timer := time.NewTimer(180 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return false
		case <-r.Context().Done():
			return true
		}
	}
	var backup atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	err := runCandidateProgressHedgedPeerQuorum(ctx, 2, 1, 2, 500*time.Millisecond, func(workCtx context.Context, index int, began, progress func()) (bool, error) {
		if index != 0 {
			backup.Add(1)
			return false, nil
		}
		began() // The production caller signals this after taking its remote slot.
		err := verifyCandidateRemoteMaterialSetsWithProgress(workCtx, primary.client, primary.server.URL, pointers, manifests, 7, progress)
		return err == nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := primary.takeRequests()
	assertCandidateRemoteSetReads(t, requests, expected)
	if len(requests) != 4 || time.Since(started) <= 500*time.Millisecond {
		t.Fatalf("fixture did not complete four progressing batches beyond hedge delay: batches=%d elapsed=%v", len(requests), time.Since(started))
	}
	if backup.Load() != 0 {
		t.Fatalf("continuous fully verified batches started redundant backup: calls=%d", backup.Load())
	}
}

func TestCandidateVerifiedProgressCompletedBatchThenStallHedges(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-headers", true: "partial-body"}[partial], func(t *testing.T) {
			primary := newCandidateRemoteSetFixture(t, 7)
			files := negotiatedMaterialItems(t, primary, 33)
			pointer, manifest := primary.source(11, files)
			pointers := map[uint64]store.MaterialManifestRecord{11: pointer}
			manifests := map[uint64]materialManifest{11: manifest}
			backup := newCandidateRemoteSetFixture(t, 8)
			copyCandidateProgressMaterials(t, primary, backup, append(files, materialBatchItem{Digest: pointer.Digest, Bytes: pointer.Bytes}))
			var requests atomic.Int32
			drained := make(chan struct{})
			primary.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				if requests.Add(1) == 1 {
					timer := time.NewTimer(300 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
						return false
					case <-r.Context().Done():
						return true
					}
				}
				defer close(drained)
				if partial {
					actual := httptest.NewRecorder()
					primary.handler.ServeHTTP(actual, r)
					if actual.Code != http.StatusOK {
						t.Errorf("actual authenticated status=%d", actual.Code)
						return true
					}
					for key, values := range actual.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(actual.Code)
					_, _ = w.Write(actual.Body.Bytes()[:actual.Body.Len()-1])
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				return true
			}
			var completed atomic.Int32
			var lastComplete atomic.Int64
			var backupStarted atomic.Int64
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := runCandidateProgressHedgedPeerQuorum(ctx, 2, 1, 2, 500*time.Millisecond, func(workCtx context.Context, index int, began, progress func()) (bool, error) {
				began()
				if index == 0 {
					err := verifyCandidateRemoteMaterialSetsWithProgress(workCtx, primary.client, primary.server.URL, pointers, manifests, 7, func() {
						completed.Add(1)
						lastComplete.Store(time.Now().UnixNano())
						progress()
					})
					return err == nil, err
				}
				backupStarted.Store(time.Now().UnixNano())
				err := verifyCandidateRemoteMaterialSets(workCtx, backup.client, backup.server.URL, pointers, manifests, 8)
				return err == nil, err
			})
			if err != nil || ctx.Err() != nil || completed.Load() != 1 {
				t.Fatalf("stall did not retain completed batch and complete backup: completed=%d err=%v caller=%v", completed.Load(), err, ctx.Err())
			}
			if gap := time.Duration(backupStarted.Load() - lastComplete.Load()); gap < 500*time.Millisecond {
				t.Fatalf("backup clock ignored complete batch: verified-to-backup=%v", gap)
			}
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("stalled actual TLS request did not drain")
			}
			if len(primary.takeRequests()) != 2 || len(backup.takeRequests()) != 2 {
				t.Fatal("missing full primary prefix or complete backup requests")
			}
		})
	}
}

func copyCandidateProgressMaterials(t *testing.T, source, target *candidateRemoteSetFixture, items []materialBatchItem) {
	t.Helper()
	for _, item := range items {
		raw, err := os.ReadFile(filepath.Join(source.root, item.Digest[:2], item.Digest))
		if err != nil {
			t.Fatal(err)
		}
		if copy := target.put(raw); copy != item {
			t.Fatal("copied peer canonical bytes differ")
		}
	}
}

func TestCandidateVerifiedProgressSlotWaitDoesNotStartClock(t *testing.T) {
	primary := newCandidateRemoteSetFixture(t, 7)
	pointers, manifests, expected := candidateRemoteTwoSources(t, primary)
	queued, releaseSlot := make(chan struct{}), make(chan struct{})
	var backup atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runCandidateProgressHedgedPeerQuorum(ctx, 2, 1, 2, 100*time.Millisecond, func(workCtx context.Context, index int, began, progress func()) (bool, error) {
			if index != 0 {
				backup.Add(1)
				return false, nil
			}
			close(queued)
			select {
			case <-releaseSlot:
			case <-workCtx.Done():
				return false, workCtx.Err()
			}
			began()
			err := verifyCandidateRemoteMaterialSetsWithProgress(workCtx, primary.client, primary.server.URL, pointers, manifests, 7, progress)
			return err == nil, err
		})
	}()
	<-queued
	select {
	case <-done:
		t.Fatal("quorum completed while primary waited for its slot")
	case <-time.After(250 * time.Millisecond):
	}
	if backup.Load() != 0 || len(primary.takeRequests()) != 0 {
		t.Fatal("slot wait launched backup or primary request")
	}
	close(releaseSlot)
	if err := <-done; err != nil || backup.Load() != 0 {
		t.Fatalf("slot release did not permit single complete peer: backup=%d err=%v", backup.Load(), err)
	}
	assertCandidateRemoteSetReads(t, primary.takeRequests(), expected)
}

func TestCandidateVerifiedProgressCancellationJoinsWaitingPeers(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller-cancel", true: "caller-deadline"}[expire], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if expire {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
			}
			defer cancel()
			var active atomic.Int32
			err := runCandidateProgressHedgedPeerQuorum(ctx, 2, 1, 2, time.Second, func(workCtx context.Context, index int, _, _ func()) (bool, error) {
				active.Add(1)
				defer active.Add(-1)
				if index != 0 {
					t.Error("unstarted primary launched backup")
				}
				if !expire {
					cancel()
				}
				<-workCtx.Done()
				return false, workCtx.Err()
			})
			cause := context.Canceled
			if expire {
				cause = context.DeadlineExceeded
			}
			if !errors.Is(err, cause) || active.Load() != 0 {
				t.Fatalf("cancellation lost or left peers: active=%d err=%v", active.Load(), err)
			}
		})
	}
}

func TestCandidateVerifiedProgressBadResponsesNeverReportCompletion(t *testing.T) {
	for _, shape := range []string{"wrong-member", "corrupt", "truncated", "cancel"} {
		t.Run(shape, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			pointers, manifests, _ := candidateRemoteTwoSources(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				body := bytes.Clone(actual.Body.Bytes())
				for key, values := range actual.Header() {
					w.Header()[key] = values
				}
				switch shape {
				case "wrong-member":
					w.Header().Set("X-Gpuworks-Material-Member", "8")
				case "corrupt":
					body[len(body)-1] ^= 1
				case "truncated":
					body = body[:len(body)-1]
				case "cancel":
					cancel()
				}
				w.WriteHeader(actual.Code)
				_, _ = w.Write(body)
				return true
			}
			completed := 0
			err := verifyCandidateRemoteMaterialSetsWithProgress(ctx, f.client, f.server.URL, pointers, manifests, 7, func() { completed++ })
			if err == nil || completed != 0 {
				t.Fatalf("unverified response reported progress: completed=%d err=%v", completed, err)
			}
		})
	}
}

func TestCandidateVerifiedProgressLateSlotEntryUsesRemainingBudget(t *testing.T) {
	primary := newCandidateRemoteSetFixture(t, 7)
	pointers, manifests, _ := candidateRemoteTwoSources(t, primary)
	backup := newCandidateRemoteSetFixture(t, 8)
	for _, requests := range manifests {
		for _, file := range requests.Files {
			copyCandidateProgressMaterials(t, primary, backup, []materialBatchItem{{Digest: file.SHA256, Bytes: file.Bytes}})
		}
	}
	for _, pointer := range pointers {
		copyCandidateProgressMaterials(t, primary, backup, []materialBatchItem{{Digest: pointer.Digest, Bytes: pointer.Bytes}})
	}
	drained := make(chan struct{})
	primary.beforeServe = func(_ http.ResponseWriter, r *http.Request) bool {
		defer close(drained)
		<-r.Context().Done()
		return true
	}
	var acquired, backupStarted atomic.Int64
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := runCandidateProgressHedgedPeerQuorum(ctx, 2, 1, 2, materialReproofInterval, func(workCtx context.Context, index int, began, progress func()) (bool, error) {
		if index == 0 {
			timer := time.NewTimer(1600 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-workCtx.Done():
				return false, workCtx.Err()
			}
			acquired.Store(time.Now().UnixNano())
			began()
			err := verifyCandidateRemoteMaterialSetsWithProgress(workCtx, primary.client, primary.server.URL, pointers, manifests, 7, progress)
			return err == nil, err
		}
		backupStarted.Store(time.Now().UnixNano())
		err := verifyCandidateRemoteMaterialSets(workCtx, backup.client, backup.server.URL, pointers, manifests, 8)
		return err == nil, err
	})
	if err != nil || ctx.Err() != nil {
		t.Fatalf("slot wait consumed hedge budget before complete backup: err=%v caller=%v", err, ctx.Err())
	}
	if waited := time.Duration(backupStarted.Load() - acquired.Load()); waited <= 0 || waited >= 250*time.Millisecond {
		t.Fatalf("late backup ignored remaining budget: waited=%v", waited)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("late silent primary did not drain")
	}
	if len(primary.takeRequests()) != 1 || len(backup.takeRequests()) != 1 {
		t.Fatal("late entry did not complete the actual authenticated backup")
	}
}

func TestCandidateVerifiedProgressLargeGETRequiresCompleteDigest(t *testing.T) {
	for _, shape := range []string{"complete", "corrupt", "truncated"} {
		t.Run(shape, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			large := f.put(bytes.Repeat([]byte("L"), int(maxMaterialBatchBytes)+1))
			small := f.put([]byte("material after complete large GET"))
			pointer, manifest := f.source(11, []materialBatchItem{large, small})
			f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				if shape == "complete" || r.Method != http.MethodGet {
					return false
				}
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != http.StatusOK {
					t.Errorf("actual large GET status=%d", actual.Code)
					return true
				}
				body := bytes.Clone(actual.Body.Bytes())
				if shape == "corrupt" {
					body[len(body)-1] ^= 1
				} else {
					body = body[:len(body)-1]
				}
				for key, values := range actual.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(actual.Code)
				_, _ = w.Write(body)
				return true
			}
			completed := 0
			err := verifyCandidateRemoteMaterialSetsWithProgress(t.Context(), f.client, f.server.URL, map[uint64]store.MaterialManifestRecord{11: pointer}, map[uint64]materialManifest{11: manifest}, 7, func() { completed++ })
			requests := f.takeRequests()
			if shape == "complete" {
				if err != nil || completed != 3 || len(requests) != 3 {
					t.Fatalf("batch/largeGET/batch completions=%d requests=%d err=%v", completed, len(requests), err)
				}
				assertCandidateRemoteSetReads(t, requests, map[string]int{pointer.Digest: 1, large.Digest: 1, small.Digest: 1})
			} else if err == nil || completed != 1 || len(requests) != 2 {
				t.Fatalf("invalid large GET renewed progress or sent next batch: completed=%d requests=%d err=%v", completed, len(requests), err)
			}
		})
	}
}

func TestCandidateVerifiedProgressRetainsConcurrentIntegrityFailure(t *testing.T) {
	entered := make(chan struct{})
	problem := errors.New("observed canonical digest differs")
	var active atomic.Int32
	err := runCandidateProgressHedgedPeerQuorum(t.Context(), 2, 1, 2, time.Millisecond, func(ctx context.Context, index int, began, progress func()) (bool, error) {
		active.Add(1)
		defer active.Add(-1)
		began()
		if index == 0 {
			<-entered
			progress() // Already released backups cannot be withdrawn by progress.
			return true, nil
		}
		close(entered)
		<-ctx.Done()
		return false, problem
	})
	if !errors.Is(err, problem) || active.Load() != 0 {
		t.Fatalf("completed vote masked integrity failure: active=%d err=%v", active.Load(), err)
	}
}

func TestCandidateVerifiedProgressMultipleRequiredPeersRemainParallel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	entered := make(chan struct{})
	var complete atomic.Int32
	err := runCandidateProgressHedgedPeerQuorum(ctx, 2, 2, 2, time.Hour, func(ctx context.Context, index int, began, progress func()) (bool, error) {
		began()
		if index == 0 {
			select {
			case <-entered:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		} else {
			close(entered)
		}
		progress()
		complete.Add(1)
		return true, nil
	})
	if err != nil || complete.Load() != 2 {
		t.Fatalf("multi-peer threshold was hedged or incomplete: complete=%d err=%v", complete.Load(), err)
	}
}
