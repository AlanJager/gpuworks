package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDownloadToCopyStartsAfterDownloadReturns(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	owner := startBlockedModelWriter(t, r, "shared", filepath.Join(r.ModelsRoot(), "owner"), false)
	task := modelWriterTask("shared", "owner/model", proto.ModelAcquireCopy, source, "")

	reconcileModelFileWithoutBlocking(t, r, task)

	assertModelTaskParked(t, r, task.Key)
	if got := owner.cancels.Load(); got != 0 {
		t.Fatalf("download was canceled %d times by the queued copy", got)
	}
	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("copy reported %d statuses before its owner returned", got)
	}

	owner.finish(t)

	if got := r.waitFile(t, task.Key, proto.MFPresent); got.State != proto.MFPresent {
		t.Fatalf("copy did not start after the download returned: %+v", got)
	}
	assertNoModelTaskWaiting(t, r)
}

func TestSupersededRetryCompletionIsNotPublished(t *testing.T) {
	r := newStorageTestRuntime(t)
	task := modelWriterTask("fenced", "fenced/model", proto.ModelAcquireDownload, "", "")
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	original := completionOnDiskForTask
	completionOnDiskForTask = func(_ *NodeRuntime, _ proto.ModelFileTask, _ string) (modelCompletion, int64, bool, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return modelCompletion{}, 0, true, nil
	}
	t.Cleanup(func() {
		completionOnDiskForTask = original
		releaseOnce.Do(func() { close(release) })
	})

	// A parked task is retried from a snapshot; a newer task replaces it while
	// the retry reads the completion marker.
	if !r.parkStorageModelFile(task, false) {
		t.Fatal("task was not parked")
	}
	finished := make(chan struct{})
	go func() {
		r.retryParkedModelFile(context.Background(), task)
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("marker read did not start")
	}
	next := task
	next.Revision = "next"
	r.parkStorageModelFile(next, false)
	releaseOnce.Do(func() { close(release) })
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded retry did not return")
	}

	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("superseded retry published %d statuses", got)
	}
	r.dlMu.Lock()
	active := len(r.downloading)
	r.dlMu.Unlock()
	if active != 0 {
		t.Fatalf("superseded retry started %d writers", active)
	}
	if parked, ok := r.parkedModelFile(task.Key); !ok || parked.Revision != "next" {
		t.Fatalf("the newer task lost its place in the queue: %+v, %v", parked, ok)
	}
}

func TestCopyStopConcurrentWithStateReplay(t *testing.T) {
	for _, test := range []struct {
		name     string
		fullSync bool
	}{
		{name: "full sync", fullSync: true},
		{name: "update"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newStorageTestRuntime(t)
			task := modelWriterTask("stop-race", "stop/race", proto.ModelAcquireCopy, "", "")
			task.DesiredState = "absent"
			raw := mustRaw(t, proto.ModelCopyStopParams{Key: task.Key, TransferID: task.TransferID})
			const rounds = 500
			start := make(chan struct{})
			errs := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < rounds; i++ {
					if _, err := r.stopModelCopy(context.Background(), raw); err != nil {
						errs <- fmt.Errorf("stop: %w", err)
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < rounds; i++ {
					if test.fullSync {
						r.OnStateSync(context.Background(), proto.DesiredState{ModelFiles: []proto.ModelFileTask{task}})
					} else {
						r.OnStateUpdate(context.Background(), proto.StateUpdate{ModelFiles: []proto.ModelFileTask{task}})
					}
				}
			}()
			close(start)
			wg.Wait()
			select {
			case err := <-errs:
				t.Fatal(err)
			default:
			}

			assertNoModelTaskWaiting(t, r)
			old := proto.ModelFileTask{Key: task.Key, Acquisition: proto.ModelAcquireCopy, TransferID: task.TransferID}
			if !r.copyTaskRunning(old, false) {
				t.Fatal("stopped transfer lost its fence during concurrent state replay")
			}
		})
	}
}

func TestStopOfEarlierTransferKeepsTheParkedReplacement(t *testing.T) {
	r := newStorageTestRuntime(t)
	owner := startBlockedModelWriter(t, r, "owner", filepath.Join(r.ModelsRoot(), "owner"), false)
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	task := modelWriterTask("replaced", "owner/model", proto.ModelAcquireCopy, source, "")
	task.TransferID = "T2"
	reconcileModelFileWithoutBlocking(t, r, task)
	assertModelTaskParked(t, r, task.Key)

	// The server stopped T1 before it replaced it with T2; the stop arrives late.
	raw := mustRaw(t, proto.ModelCopyStopParams{Key: task.Key, TransferID: "T1"})
	if _, err := r.stopModelCopy(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	assertModelTaskParked(t, r, task.Key)

	// Stopping the parked transfer itself does withdraw it.
	raw = mustRaw(t, proto.ModelCopyStopParams{Key: task.Key, TransferID: "T2"})
	if _, err := r.stopModelCopy(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	assertNoModelTaskWaiting(t, r)
	owner.finish(t)
	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("withdrawn copy reported %d statuses", got)
	}
}

func TestCopyReplayUsesTransferIdentity(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "source")
	task := modelWriterTask("copy-replay", "copy/replay", proto.ModelAcquireCopy, source, "")
	owner := startBlockedModelWriter(t, r, task.Key, filepath.Join(r.ModelsRoot(), "copy", "replay"), true)
	r.dlMu.Lock()
	owner.active.transferID = task.TransferID
	r.dlMu.Unlock()

	replay := task
	replay.SourcePath = filepath.Join(t.TempDir(), "different")
	replay.RateLimitKBs = 1024
	reconcileModelFileWithoutBlocking(t, r, replay)
	if got := owner.cancels.Load(); got != 0 {
		t.Fatalf("replay of the same transfer canceled it %d times", got)
	}
	assertNoModelTaskWaiting(t, r)

	changed := replay
	changed.TransferID = "next-transfer"
	reconcileModelFileWithoutBlocking(t, r, changed)
	if got := owner.cancels.Load(); got != 1 {
		t.Fatalf("a new TransferID canceled the running copy %d times, want 1", got)
	}
	assertModelTaskParked(t, r, task.Key)
	r.dropWaiting(task.Key) // the replacement would otherwise start while the test cleans up
}
