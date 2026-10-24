package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (r *storageTestRuntime) waitingKeys() []string {
	r.storage.mu.RLock()
	defer r.storage.mu.RUnlock()
	var keys []string
	for key := range r.storage.waiting {
		keys = append(keys, key)
	}
	return keys
}

func (r *storageTestRuntime) fileStatusCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, st := range r.files {
		if st.Key == key {
			n++
		}
	}
	return n
}

// parkedTask is a download-less task for a storage that has not been checked
// yet: it waits, and would write into the storage once it is ready.
func parkedTask(key string, storageID uint, target string) proto.ModelFileTask {
	return proto.ModelFileTask{Key: key, StorageID: storageID, TargetPath: target, DesiredState: "present",
		Acquisition: proto.ModelAcquireRegistered, Source: "local", SourceID: target}
}

func TestParkedTaskIsDroppedWhenItsModelIsWithdrawn(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	task := parkedTask("f7", 2, "Qwen/a")
	r.reconcileModelFile(context.Background(), task)
	if got := r.waitingKeys(); len(got) != 1 {
		t.Fatalf("task was not parked: %v", got)
	}

	// The model was deleted meanwhile: the server now says absent.
	task.DesiredState = "absent"
	r.reconcileModelFile(context.Background(), task)
	if got := r.waitingKeys(); len(got) != 0 {
		t.Fatalf("a withdrawn task is still parked: %v", got)
	}
	<-r.storage.trigger // the park asked for a check; run it as the loop would
	r.checkStorages(context.Background(), true)
	if n := r.fileStatusCount("f7"); n != 0 {
		t.Fatalf("the storage recovered and the withdrawn task ran: %d statuses", n)
	}
}

func TestParkedTaskIsDroppedWhenTheFullStateNoLongerListsIt(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.reconcileModelFile(context.Background(), parkedTask("f7", 2, "Qwen/a"))
	r.reconcileModelFile(context.Background(), parkedTask("f8", 2, "Qwen/b"))
	r.OnStateSync(context.Background(), proto.DesiredState{Storages: []proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}},
		ModelFiles: []proto.ModelFileTask{parkedTask("f8", 2, "Qwen/b")}})
	if got := r.waitingKeys(); len(got) != 1 || got[0] != "f8" {
		t.Fatalf("parked after a full state that lists only f8: %v", got)
	}
}

func TestParkedTaskIsDroppedWhenTheModelIsEvicted(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared, cache := t.TempDir(), filepath.Join(t.TempDir(), "cache")
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 3, Name: "c", Type: proto.StorageNFSCache, Path: shared, Existing: true, CachePath: cache}})
	<-r.storage.trigger
	// The cache copy of a model whose storage has not been checked yet.
	task := parkedTask("f9", 3, "modelscope/m")
	task.Cache = true
	r.reconcileModelFile(context.Background(), task)
	if got := r.waitingKeys(); len(got) != 1 {
		t.Fatalf("task was not parked: %v", got)
	}
	other := parkedTask("f10", 3, "modelscope/other")
	other.Cache = true
	r.reconcileModelFile(context.Background(), other)

	evicted := filepath.Join(cache, "modelscope", "m")
	if _, err := r.rpcModelEvict(context.Background(), mustRaw(t, map[string]string{"path": evicted})); err != nil {
		t.Fatal(err)
	}
	if got := r.waitingKeys(); len(got) != 1 || got[0] != "f10" {
		t.Fatalf("parked after evicting one model: %v", got)
	}
}

func TestEvictedWriterSnapshotCannotRecreateModel(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "new weights")
	task := modelWriterTask("snapshot", "org/model", proto.ModelAcquireCopy, source, "")
	intent := r.beginModelFileIntent(task)
	if !r.parkWriterModelFile(intent) {
		t.Fatal("writer was not parked")
	}
	r.storage.mu.RLock()
	snapshot, ok := r.storage.writerWaiting[task.Key]
	r.storage.mu.RUnlock()
	if !ok || snapshot.generation != intent.generation {
		t.Fatalf("writer snapshot = %+v, %v; want generation %d", snapshot, ok, intent.generation)
	}
	target := filepath.Join(r.ModelsRoot(), "org", "model")
	writeModel(t, target, "old weights")
	if _, err := r.rpcModelEvict(context.Background(), evictParams(target)); err != nil {
		t.Fatal(err)
	}

	// This is the intent wakeModelWriters copied before eviction. Its first
	// current check must reject it before it can register a new writer.
	r.reconcileParkedModelFile(context.Background(), snapshot)
	r.dlMu.Lock()
	active := r.downloading[task.Key]
	r.dlMu.Unlock()
	if active != nil {
		select {
		case <-active.done:
		case <-time.After(5 * time.Second):
			t.Fatal("evicted snapshot writer did not return")
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("evicted snapshot recreated its target: %v", err)
	}
	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("evicted snapshot reported %d statuses", got)
	}

	next := task
	next.TransferID = "next-generation"
	r.reconcileModelFile(context.Background(), next)
	if got := r.waitFile(t, task.Key, proto.MFPresent); got.TransferID != next.TransferID {
		t.Fatalf("new generation status = %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(target, "model.safetensors"))
	if err != nil || string(data) != "new weights" {
		t.Fatalf("new generation contents = %q, err=%v", data, err)
	}
	r.dlMu.Lock()
	active = r.downloading[task.Key]
	r.dlMu.Unlock()
	if active != nil {
		select {
		case <-active.done:
		case <-time.After(5 * time.Second):
			t.Fatal("new generation writer did not unregister")
		}
	}
	assertNoModelTaskWaiting(t, r)
}

func TestStorageCheckSnapshotIsFencedByEviction(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared, cache := t.TempDir(), filepath.Join(t.TempDir(), "cache")
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 3, Name: "c", Type: proto.StorageNFSCache, Path: shared, Existing: true, CachePath: cache}})
	<-r.storage.trigger
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	task := modelWriterTask("storage-snapshot", "org/model", proto.ModelAcquireCopy, source, "")
	task.StorageID, task.Cache = 3, true
	r.reconcileModelFile(context.Background(), task)
	if got := r.waitingKeys(); len(got) != 1 {
		t.Fatalf("task was not parked for its storage: %v", got)
	}
	target := filepath.Join(cache, "org", "model")
	writeModel(t, target, "old weights")

	original := completionOnDiskForIntent
	started, release := make(chan struct{}), make(chan struct{})
	released := false
	releaseRead := func() {
		if !released {
			close(release)
			released = true
		}
	}
	completionOnDiskForIntent = func(n *NodeRuntime, mf proto.ModelFileTask, dir string) (modelCompletion, int64, bool, error) {
		if mf.Key != task.Key {
			return original(n, mf, dir)
		}
		close(started)
		<-release
		return modelCompletion{}, 0, false, nil
	}
	t.Cleanup(func() {
		releaseRead()
		completionOnDiskForIntent = original
	})
	checked := make(chan struct{})
	go func() {
		r.checkStorages(context.Background(), true)
		close(checked)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("storage-check snapshot did not reach its marker read")
	}
	if _, err := r.rpcModelEvict(context.Background(), evictParams(target)); err != nil {
		t.Fatal(err)
	}
	releaseRead()
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("storage-check snapshot did not return")
	}

	// The snapshot was already past its first current check when eviction ran.
	// Its second check, immediately before active registration, must fence it.
	r.dlMu.Lock()
	active := r.downloading[task.Key]
	r.dlMu.Unlock()
	if active != nil {
		select {
		case <-active.done:
		case <-time.After(5 * time.Second):
			t.Fatal("storage-check snapshot writer did not return")
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("storage-check snapshot recreated its target: %v", err)
	}
	if got := r.fileStatusCount(task.Key); got != 0 {
		t.Fatalf("storage-check snapshot reported %d statuses", got)
	}
	assertNoModelTaskWaiting(t, r)
}

func TestParkedTaskIsDroppedWhenItsStorageIsRemoved(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	keep := t.TempDir()
	r.applyStorages([]proto.StorageTask{
		{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared},
		{ID: 4, Name: "keep", Type: proto.StorageSharedDir, Path: keep},
	})
	<-r.storage.trigger
	r.reconcileModelFile(context.Background(), parkedTask("f7", 2, "Qwen/a"))
	r.reconcileModelFile(context.Background(), parkedTask("f8", 4, "Qwen/b"))
	r.applyStorages([]proto.StorageTask{{ID: 4, Name: "keep", Type: proto.StorageSharedDir, Path: keep}})
	if got := r.waitingKeys(); len(got) != 1 || got[0] != "f8" {
		t.Fatalf("parked after storage 2 was removed: %v", got)
	}
}

// A write task for a storage that is ready but read-only here can never run;
// it is reported instead of waiting forever.
func TestWriteTaskOnAReadOnlyStorageFails(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	// A file where the write probe wants its directory: the share rejects writes.
	if err := os.WriteFile(filepath.Join(shared, storagemount.MetaDirName), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	if st := r.storage.statuses[2]; st.State != storagemount.StateReady || st.Writable {
		t.Fatalf("setup: want a ready, read-only storage, got %+v", st)
	}
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	task := proto.ModelFileTask{Key: "f7", StorageID: 2, TargetPath: "Qwen/a", DesiredState: "present", Acquisition: proto.ModelAcquireCopy,
		SourcePath: source, Staging: proto.StagingRelPath("cp", "Qwen/a"), Source: "local", SourceID: "Qwen/a"}
	r.reconcileModelFile(context.Background(), task)
	if got := r.lastFile(t, "f7"); got.State != proto.MFFailed || got.Message != "storage is read-only on this node" {
		t.Fatalf("write task on a read-only storage = %+v", got)
	}
	if got := r.waitingKeys(); len(got) != 0 {
		t.Fatalf("the task waits for a storage that will not become writable: %v", got)
	}
}

// The node cache of an nfs_cache storage is a local directory: a read-only
// share does not stop it from filling.
func TestCacheTaskOnAReadOnlyShareStillRuns(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared, cache := t.TempDir(), filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(filepath.Join(shared, storagemount.MetaDirName), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 3, Name: "c", Type: proto.StorageNFSCache, Path: shared, Existing: true, CachePath: cache}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	root, ready, err := r.storageRoot(proto.ModelFileTask{StorageID: 3, Cache: true})
	if err != nil || !ready || root != cache {
		t.Fatalf("cache root = %q ready=%v err=%v", root, ready, err)
	}
}

// The agent has no default directories: the server always sends them.
func TestStorageWithoutADirectoryFails(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{
		{ID: 1, Name: "default", Type: proto.StorageLocal},
		{ID: 3, Name: "cached", Type: proto.StorageNFSCache, Path: shared, Existing: true},
	})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	for _, id := range []uint{1, 3} {
		st := r.storage.statuses[id]
		if st.State != proto.StorageStateFailed || st.Reason != storagemount.ReasonPathMissing || st.Writable {
			t.Errorf("storage %d = %+v, want failed/path_missing", id, st)
		}
	}
	if _, err := os.Stat(r.ModelsRoot()); !os.IsNotExist(err) {
		t.Errorf("the models root was used as the storage's directory: %v", err)
	}
	// Tasks for them are reported, not run against a made-up directory.
	r.reconcileModelFile(context.Background(), proto.ModelFileTask{Key: "f1", StorageID: 1, TargetPath: "m", DesiredState: "present"})
	if got := r.lastFile(t, "f1"); got.State != proto.MFFailed || !strings.Contains(got.Message, "no directory") {
		t.Errorf("task on a local storage without a directory = %+v", got)
	}
	cacheTask := proto.ModelFileTask{Key: "f3", StorageID: 3, TargetPath: "m", DesiredState: "present", Cache: true}
	r.reconcileModelFile(context.Background(), cacheTask)
	if got := r.lastFile(t, "f3"); got.State != proto.MFFailed || !strings.Contains(got.Message, "no cache directory") {
		t.Errorf("cache task without a cache directory = %+v", got)
	}
}

// A shared directory that is a bare directory on the local disk is not a share.
func TestSharedDirOnTheLocalDiskIsNotMounted(t *testing.T) {
	r := newStorageTestRuntime(t)
	bare := t.TempDir()
	r.fs.onLocalDisk(bare)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: bare}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	st := r.storage.statuses[2]
	if st.State != proto.StorageStateFailed || st.Reason != storagemount.ReasonNotMounted || st.Writable {
		t.Fatalf("shared_dir on a local directory = %+v", st)
	}
	// Its tasks wait; nothing is written to the local disk.
	r.reconcileModelFile(context.Background(), parkedTask("f7", 2, "Qwen/a"))
	r.checkStorages(context.Background(), false)
	if entries, _ := os.ReadDir(bare); len(entries) != 0 {
		t.Fatalf("something was written to the unmounted directory: %v", entries)
	}
}
