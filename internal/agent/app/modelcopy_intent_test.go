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

func TestSupersededCompletionIsNotPublished(t *testing.T) {
	r := newStorageTestRuntime(t)
	task := modelWriterTask("fenced", "fenced/model", proto.ModelAcquireDownload, "", "")
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	original := completionOnDiskForIntent
	completionOnDiskForIntent = func(_ *NodeRuntime, _ proto.ModelFileTask, _ string) (modelCompletion, int64, bool, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return modelCompletion{}, 0, true, nil
	}
	t.Cleanup(func() {
		completionOnDiskForIntent = original
		releaseOnce.Do(func() { close(release) })
	})

	finished := make(chan struct{})
	go func() {
		r.reconcileModelFile(context.Background(), task)
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("marker read did not start")
	}
	r.beginModelFileIntent(task)
	releaseOnce.Do(func() { close(release) })
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded completion did not return")
	}

	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("superseded completion published %d statuses", got)
	}
	r.dlMu.Lock()
	active := len(r.downloading)
	r.dlMu.Unlock()
	if active != 0 {
		t.Fatalf("superseded completion started %d writers", active)
	}
}

func TestNewIntentWaitsForInFlightStatusBeforeFencing(t *testing.T) {
	r := newStorageTestRuntime(t)
	task := modelWriterTask("fenced", "fenced/model", proto.ModelAcquireDownload, "", "")
	first := r.beginModelFileIntent(task)
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	var sentMu sync.Mutex
	var states []string
	r.Bind(func(kind string, value any) error {
		if kind != proto.TStatusModelFile {
			return nil
		}
		st := value.(proto.ModelFileStatus)
		if st.Key == task.Key {
			startedOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				return fmt.Errorf("timed out waiting to release test status")
			}
			sentMu.Lock()
			states = append(states, st.State)
			sentMu.Unlock()
		}
		return nil
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	go r.reportModelFileWithIntent(context.Background(), first, proto.ModelFileStatus{Key: task.Key, State: proto.MFDownloading})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first status was not sent")
	}
	secondDone := make(chan modelFileIntent, 1)
	go func() { secondDone <- r.beginModelFileIntent(task) }()
	select {
	case <-secondDone:
		t.Fatal("new intent did not wait for the in-flight status")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	var second modelFileIntent
	select {
	case second = <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("new intent remained blocked after the status completed")
	}

	r.reportModelFileWithIntent(context.Background(), first, proto.ModelFileStatus{Key: task.Key, State: proto.MFPresent})
	r.reportModelFileWithIntent(context.Background(), second, proto.ModelFileStatus{Key: task.Key, State: proto.MFPresent})
	sentMu.Lock()
	defer sentMu.Unlock()
	if len(states) != 2 || states[0] != proto.MFDownloading || states[1] != proto.MFPresent {
		t.Fatalf("status sequence = %v, want downloading then present without a stale completion", states)
	}
}
