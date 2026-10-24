package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

type blockedModelWriter struct {
	active      *modelDownload
	release     chan struct{}
	releaseOnce sync.Once
	cancels     atomic.Int64
}

func startBlockedModelWriter(t *testing.T, r *storageTestRuntime, key, target string, copyWriter bool) *blockedModelWriter {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	run := func(context.Context) { close(started); <-release }
	r.dlMu.Lock()
	if copyWriter {
		r.startTransferLocked(context.Background(), key, target, run, "owner-transfer")
	} else {
		r.startTransferLocked(context.Background(), key, target, run)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("owner did not start")
	}

	writer := &blockedModelWriter{release: release}
	r.dlMu.Lock()
	writer.active = r.downloading[key]
	if writer.active == nil {
		r.dlMu.Unlock()
		close(release)
		t.Fatal("owner is not registered")
	}
	cancel := writer.active.cancel
	writer.active.cancel = func() {
		writer.cancels.Add(1)
		cancel()
	}
	r.dlMu.Unlock()
	t.Cleanup(func() { writer.finish(t) })
	return writer
}

func (w *blockedModelWriter) finish(t *testing.T) {
	t.Helper()
	w.releaseOnce.Do(func() { close(w.release) })
	select {
	case <-w.active.done:
	case <-time.After(5 * time.Second):
		t.Error("owner did not return")
	}
}

func reconcileModelFileWithoutBlocking(t *testing.T, r *storageTestRuntime, task proto.ModelFileTask) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan struct{})
	go func() {
		r.reconcileModelFile(ctx, task)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		cancel()
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("reconcile remained blocked after cancellation")
		}
		t.Fatal("reconcile waited for the active owner")
	}
}

func assertOnlyModelWriter(t *testing.T, r *storageTestRuntime, key string) {
	t.Helper()
	r.dlMu.Lock()
	defer r.dlMu.Unlock()
	if len(r.downloading) != 1 || r.downloading[key] == nil {
		t.Fatalf("active writers = %v, want only %q", r.downloading, key)
	}
}

func assertModelTaskParked(t *testing.T, r *storageTestRuntime, key string) {
	t.Helper()
	r.storage.mu.RLock()
	_, writerWaiting := r.storage.writerWaiting[key]
	writerCount := len(r.storage.writerWaiting)
	r.storage.mu.RUnlock()
	if !writerWaiting || writerCount != 1 {
		t.Fatalf("writer waiting for %q = %v, count = %d", key, writerWaiting, writerCount)
	}
	if got := r.waitingKeys(); len(got) != 0 {
		t.Fatalf("writer task leaked into storage queue: %v", got)
	}
}

func assertNoModelTaskWaiting(t *testing.T, r *storageTestRuntime) {
	t.Helper()
	r.storage.mu.RLock()
	writers := len(r.storage.writerWaiting)
	r.storage.mu.RUnlock()
	if writers != 0 || len(r.waitingKeys()) != 0 {
		t.Fatalf("waiting tasks remain: storage=%v writer=%d", r.waitingKeys(), writers)
	}
}

func modelWriterTask(key, target, acquisition, copySource, endpoint string) proto.ModelFileTask {
	task := proto.ModelFileTask{
		Key: key, TargetPath: target, DesiredState: "present", Acquisition: acquisition,
		Source: "huggingface", SourceID: "org/model", Revision: "main", Endpoint: endpoint,
	}
	if acquisition == proto.ModelAcquireCopy {
		task.Source = "local"
		task.SourcePath = copySource
		task.TransferID = key + "-transfer"
	}
	return task
}

func newModelDownloadFixture(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	config := []byte(`{"architectures":["Qwen3ForCausalLM"]}`)
	weights := []byte("weights")
	files := map[string][]byte{
		"/org/model/resolve/main/config.json":       config,
		"/org/model/resolve/main/model.safetensors": weights,
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/models/org/model/tree/main" {
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model.safetensors","size":%d}]`, len(config), len(weights))
			return
		}
		if body, ok := files[r.URL.Path]; ok {
			http.ServeContent(w, r, "model", time.Time{}, bytes.NewReader(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL, &requests
}

func TestReconcileParksOverlappingWriters(t *testing.T) {
	relations := []struct {
		name             string
		owner, contender string
	}{
		{name: "same", owner: "org/model", contender: "org/model"},
		{name: "owner parent", owner: "org", contender: "org/model"},
		{name: "owner child", owner: "org/model", contender: "org"},
	}
	pairs := []struct {
		name      string
		ownerCopy bool
		contender string
	}{
		{name: "download then copy", contender: proto.ModelAcquireCopy},
		{name: "download then download", contender: proto.ModelAcquireDownload},
		{name: "copy then copy", ownerCopy: true, contender: proto.ModelAcquireCopy},
		{name: "copy then download", ownerCopy: true, contender: proto.ModelAcquireDownload},
	}
	for _, relation := range relations {
		for _, pair := range pairs {
			t.Run(relation.name+"/"+pair.name, func(t *testing.T) {
				r := newStorageTestRuntime(t)
				source := filepath.Join(t.TempDir(), "source")
				writeModel(t, source, "weights")
				endpoint, requests := newModelDownloadFixture(t)
				owner := startBlockedModelWriter(t, r, "owner", filepath.Join(r.ModelsRoot(), filepath.FromSlash(relation.owner)), pair.ownerCopy)
				task := modelWriterTask("contender", relation.contender, pair.contender, source, endpoint)

				reconcileModelFileWithoutBlocking(t, r, task)

				if got := owner.cancels.Load(); got != 0 {
					t.Fatalf("owner canceled %d times", got)
				}
				assertOnlyModelWriter(t, r, "owner")
				assertModelTaskParked(t, r, task.Key)
				if got := requests.Load(); got != 0 {
					t.Fatalf("contender made %d HTTP requests", got)
				}
				if got := r.fileStatusCount(task.Key); got != 0 {
					t.Fatalf("contender reported %d statuses without a writer", got)
				}
				r.dropWaiting(task.Key)
			})
		}
	}
}

func TestReconcileReadOnlyTasksDoNotCancelWriter(t *testing.T) {
	for _, acquisition := range []string{proto.ModelAcquireRegistered, proto.ModelAcquirePeer} {
		t.Run(acquisition, func(t *testing.T) {
			r := newStorageTestRuntime(t)
			target := filepath.Join(r.ModelsRoot(), "org", "model")
			writeModel(t, target, "weights")
			owner := startBlockedModelWriter(t, r, "owner", target, true)
			task := proto.ModelFileTask{
				Key: "reader", TargetPath: "org/model", DesiredState: "present",
				Acquisition: acquisition, TransferID: "peer-transfer",
			}

			reconcileModelFileWithoutBlocking(t, r, task)

			if got := owner.cancels.Load(); got != 0 {
				t.Fatalf("owner canceled %d times", got)
			}
			assertOnlyModelWriter(t, r, "owner")
			if got := r.lastFile(t, task.Key); got.State != proto.MFPresent || got.Acquisition != acquisition {
				t.Fatalf("read-only status = %+v", got)
			}
			if got := r.waitingKeys(); len(got) != 0 {
				t.Fatalf("read-only task was parked: %v", got)
			}
		})
	}
}

func TestLocalOverlappingWriterStartsAfterOwnerReturnsWithoutStorageList(t *testing.T) {
	for _, acquisition := range []string{proto.ModelAcquireCopy, proto.ModelAcquireDownload} {
		t.Run(acquisition, func(t *testing.T) {
			r := newStorageTestRuntime(t)
			r.storage.mu.RLock()
			known := r.storage.known
			r.storage.mu.RUnlock()
			if known {
				t.Fatal("test requires an old server with no storage list")
			}
			source := filepath.Join(t.TempDir(), "source")
			writeModel(t, source, "weights")
			endpoint, requests := newModelDownloadFixture(t)
			owner := startBlockedModelWriter(t, r, "owner", filepath.Join(r.ModelsRoot(), "org"), acquisition == proto.ModelAcquireDownload)
			task := modelWriterTask("contender", "org/model", acquisition, source, endpoint)
			reconcileModelFileWithoutBlocking(t, r, task)
			assertOnlyModelWriter(t, r, "owner")

			owner.finish(t)

			got := r.waitFile(t, task.Key, proto.MFPresent)
			wantPath := filepath.Join(r.ModelsRoot(), "org", "model")
			gotPath, err := filepath.EvalSymlinks(got.Path)
			if err != nil {
				t.Fatal(err)
			}
			wantPath, err = filepath.EvalSymlinks(wantPath)
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != wantPath {
				t.Fatalf("present path = %q, want %q", gotPath, wantPath)
			}
			data, err := os.ReadFile(filepath.Join(wantPath, "model.safetensors"))
			if err != nil || string(data) != "weights" {
				t.Fatalf("retried writer contents = %q, err=%v", data, err)
			}
			if owner.cancels.Load() != 0 {
				t.Fatal("owner was canceled while contender parked")
			}
			if acquisition == proto.ModelAcquireDownload && requests.Load() == 0 {
				t.Fatal("retried download made no HTTP requests")
			}
			assertNoModelTaskWaiting(t, r)
		})
	}
}

func TestCopyConcurrencyLimitParksAdditionalWriter(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	owners := make([]*blockedModelWriter, 0, maxConcurrentModelCopies)
	for i := 0; i < maxConcurrentModelCopies; i++ {
		key := fmt.Sprintf("owner-%d", i)
		target := filepath.Join(r.ModelsRoot(), fmt.Sprintf("model-%d", i))
		owners = append(owners, startBlockedModelWriter(t, r, key, target, true))
	}
	task := modelWriterTask("overflow", "overflow", proto.ModelAcquireCopy, source, "")

	reconcileModelFileWithoutBlocking(t, r, task)

	r.dlMu.Lock()
	active := len(r.downloading)
	r.dlMu.Unlock()
	if active != maxConcurrentModelCopies {
		t.Fatalf("active writers = %d, want copy limit %d", active, maxConcurrentModelCopies)
	}
	assertModelTaskParked(t, r, task.Key)
	for _, owner := range owners {
		if got := owner.cancels.Load(); got != 0 {
			t.Fatalf("owner canceled %d times at copy limit", got)
		}
	}
	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("overflow copy reported %d statuses without a writer", got)
	}

	owners[0].finish(t)
	if got := r.waitFile(t, task.Key, proto.MFPresent); got.State != proto.MFPresent {
		t.Fatalf("overflow copy did not start after a slot was released: %+v", got)
	}
	assertNoModelTaskWaiting(t, r)
	for _, owner := range owners {
		if got := owner.cancels.Load(); got != 0 {
			t.Fatalf("owner canceled %d times while releasing a copy slot", got)
		}
	}
}

func TestStaleStorageRetryCannotCancelNewCopyGeneration(t *testing.T) {
	r := newStorageTestRuntime(t)
	if err := os.MkdirAll(r.ModelsRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(r.ModelsRoot())
	if err != nil {
		t.Fatal(err)
	}
	r.applyStorages([]proto.StorageTask{{ID: 1, Name: "local", Type: proto.StorageLocal, Path: root}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	r.storage.mu.RLock()
	storageStatus := r.storage.statuses[1]
	r.storage.mu.RUnlock()
	if storageStatus.State != storagemount.StateReady {
		t.Fatalf("local storage status = %+v", storageStatus)
	}

	sourceB1 := filepath.Join(t.TempDir(), "source-b1")
	writeModel(t, sourceB1, string(bytes.Repeat([]byte("b"), 128<<10)))
	sourceB2 := filepath.Join(t.TempDir(), "source-b2")
	writeModel(t, sourceB2, string(bytes.Repeat([]byte("c"), 128<<10)))
	task := modelWriterTask("generation", "owner/model", proto.ModelAcquireCopy, sourceB1, "")
	task.StorageID = 1
	task.SourceID = "org/b1"
	task.TransferID = "B1"
	task.RateLimitKBs = 128
	owner := startBlockedModelWriter(t, r, "owner", filepath.Join(root, "owner"), true)
	r.reconcileModelFile(context.Background(), task)
	assertModelTaskParked(t, r, task.Key)
	r.storage.mu.RLock()
	b1 := r.storage.writerWaiting[task.Key]
	r.storage.mu.RUnlock()
	if b1.generation == 0 {
		t.Fatal("B1 has no generation")
	}

	owner.finish(t)
	r.waitFile(t, task.Key, proto.MFDownloading)
	assertNoModelTaskWaiting(t, r)

	task.SourcePath = sourceB2
	task.SourceID = "org/b2"
	task.TransferID = "B2"
	task.RateLimitKBs = 64
	r.reconcileModelFile(context.Background(), task)
	var b2Active *modelDownload
	var b2Cancels atomic.Int64
	deadline := time.Now().Add(10 * time.Second)
	for b2Active == nil {
		r.dlMu.Lock()
		active := r.downloading[task.Key]
		if active != nil && active.transferID == "B2" {
			b2Active = active
			cancelB2 := b2Active.cancel
			b2Active.cancel = func() {
				b2Cancels.Add(1)
				cancelB2()
			}
		}
		r.dlMu.Unlock()
		if b2Active == nil {
			if time.Now().After(deadline) {
				t.Fatal("B2 did not start after B1 returned")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	r.storage.mu.Lock()
	r.storage.waiting[task.Key] = b1.task
	r.storage.waitingGeneration[task.Key] = b1.generation
	r.storage.mu.Unlock()
	r.checkStorages(context.Background(), true)

	if got := b2Cancels.Load(); got != 0 {
		t.Fatalf("stale B1 canceled B2 %d times", got)
	}
	r.dlMu.Lock()
	active := r.downloading[task.Key]
	activeCount := len(r.downloading)
	r.dlMu.Unlock()
	if active != b2Active || activeCount != 1 {
		t.Fatalf("stale B1 replaced B2: active=%p want=%p count=%d", active, b2Active, activeCount)
	}
	assertNoModelTaskWaiting(t, r)
	select {
	case <-b2Active.done:
	case <-time.After(15 * time.Second):
		t.Fatal("B2 did not finish")
	}
	if got := r.lastFile(t, task.Key); got.State != proto.MFPresent || got.TransferID != "B2" {
		t.Fatalf("B2 final status = %+v", got)
	}
	if got := b2Cancels.Load(); got != 0 {
		t.Fatalf("B2 was canceled before completion: %d", got)
	}
	assertNoModelTaskWaiting(t, r)
}

func TestWriterWakeStillRequiresNonZeroStorageReady(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "shared", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	r.storage.mu.RLock()
	initialStatus := r.storage.statuses[2]
	r.storage.mu.RUnlock()
	if initialStatus.State != storagemount.StateReady || !initialStatus.Writable {
		t.Fatalf("shared storage is not ready: %+v", initialStatus)
	}
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	task := modelWriterTask("contender", "org/model", proto.ModelAcquireCopy, source, "")
	task.StorageID = 2
	owner := startBlockedModelWriter(t, r, "owner", filepath.Join(shared, "org"), true)
	r.reconcileModelFile(context.Background(), task)
	assertModelTaskParked(t, r, task.Key)

	r.storage.mu.Lock()
	status := r.storage.statuses[2]
	status.State = storagemount.StateFailed
	r.storage.statuses[2] = status
	r.storage.mu.Unlock()
	owner.finish(t)

	deadline := time.Now().Add(5 * time.Second)
	for {
		r.storage.mu.RLock()
		_, storageWaiting := r.storage.waiting[task.Key]
		_, writerWaiting := r.storage.writerWaiting[task.Key]
		r.storage.mu.RUnlock()
		if storageWaiting && !writerWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("non-ready storage task did not return to the storage queue")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.dlMu.Lock()
	active := len(r.downloading)
	r.dlMu.Unlock()
	if active != 0 || r.fileStatusCount(task.Key) != 0 {
		t.Fatalf("non-ready task started a writer: active=%d statuses=%d", active, r.fileStatusCount(task.Key))
	}

	select {
	case <-r.storage.trigger:
	default:
	}
	r.checkStorages(context.Background(), true)
	r.waitFile(t, task.Key, proto.MFPresent)
	assertNoModelTaskWaiting(t, r)
}
