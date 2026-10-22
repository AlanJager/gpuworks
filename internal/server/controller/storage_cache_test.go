package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type cacheFailureHub struct {
	*storageHub
	fail   bool
	cancel bool
	hook   func()
	empty  bool
}

func (h *cacheFailureHub) CallNode(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
	if h.hook != nil {
		h.hook()
	}
	if h.cancel {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if h.fail {
		return nil, errors.New("injected unlink failure")
	}
	if h.empty {
		return json.RawMessage(`{}`), nil
	}
	if _, err := h.storageHub.CallNode(ctx, uuid, method, params); err != nil {
		return nil, err
	}
	path := params.(map[string]string)["path"]
	raw, err := json.Marshal(map[string]any{"path": path, "removed": true, "eviction_id": params.(map[string]string)["eviction_id"]})
	return raw, err
}

func cacheFile(t *testing.T, f *storageControllerFixture, name string, size int64, backed bool) *store.ModelFile {
	t.Helper()
	model, err := f.st.UpsertModel(&store.Model{Source: "local", SourceID: name, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if backed {
		if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: f.cached.ID, State: proto.MFPresent, Path: name, SizeBytes: size}); err != nil {
			t.Fatal(err)
		}
	}
	row, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: f.cached.ID, NodeID: f.node.ID, State: proto.MFPresent, Path: f.cached.CachePath + "/" + name, SizeBytes: size})
	if err != nil {
		t.Fatal(err)
	}
	return row
}
func prepareCache(t *testing.T, f *storageControllerFixture, cap int64) {
	t.Helper()
	var err error
	f.cached, err = f.st.UpdateStorage(f.cached.ID, store.StoragePatch{CacheMaxBytes: &cap})
	if err != nil {
		t.Fatal(err)
	}
	f.c.hub = &cacheFailureHub{storageHub: f.hub}
	f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, CachePath: f.cached.CachePath, Path: f.cached.Path})
}
func runCache(t *testing.T, f *storageControllerFixture, mode string) {
	t.Helper()
	if err := f.st.RequestCacheOperations(f.cached.ID, []uint{f.node.ID}, mode, true); err != nil {
		t.Fatal(err)
	}
	if err := f.c.reconcileStorageCache(context.Background(), f.cached.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCacheTrimPartialAndFailureRetry(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 200)
	safe := cacheFile(t, f, "safe", 60, true)
	protected := cacheFile(t, f, "unbacked", 240, false)
	h := &cacheFailureHub{storageHub: f.hub, fail: true}
	f.c.hub = h
	runCache(t, f, "trim")
	op, _ := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if op.State != "failed" || op.Error != "injected unlink failure" {
		t.Fatalf("failed=%+v", op)
	}
	if _, err := f.st.ModelFileByID(safe.ID); err != nil {
		t.Fatal("RPC failure removed index")
	}
	h.fail = false
	runCache(t, f, "trim")
	if err := f.c.reconcileStorageCache(t.Context(), f.cached.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ModelFileByID(safe.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("safe row survived: %v", err)
	}
	if _, err := f.st.ModelFileByID(protected.ID); err != nil {
		t.Fatal("protected row removed")
	}
	op, _ = f.st.CacheOperation(f.cached.ID, f.node.ID)
	if op.State != "waiting" {
		t.Fatalf("blocked=%+v", op)
	}
}

func TestCacheOfflineThenRecoveryAndConsumedClear(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 1000)
	safe := cacheFile(t, f, "safe", 60, true)
	f.hub.online[f.node.UUID] = false
	runCache(t, f, "clear")
	if _, err := f.st.ModelFileByID(safe.ID); err != nil {
		t.Fatal("offline index removed")
	}
	f.hub.online[f.node.UUID] = true
	if err := f.c.reconcileStorageCache(context.Background(), f.cached.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
	op, _ := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if op.State != "completed" {
		t.Fatalf("clear=%+v", op)
	}
	next := cacheFile(t, f, "next", 60, true)
	if err := f.st.RequestCacheOperations(f.cached.ID, []uint{f.node.ID}, "trim", false); err != nil {
		t.Fatal(err)
	}
	if err := f.c.reconcileStorageCache(context.Background(), f.cached.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ModelFileByID(next.ID); err != nil {
		t.Fatal("completed clear replayed")
	}
}

func TestCacheInFlightGenerationCannotCompleteNewRequest(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 20)
	cacheFile(t, f, "safe", 60, true)
	h := &cacheFailureHub{storageHub: f.hub}
	f.c.hub = h
	h.hook = func() {
		if err := f.st.RequestCacheOperations(f.cached.ID, []uint{f.node.ID}, "clear", true); err != nil {
			t.Fatal(err)
		}
	}
	runCache(t, f, "trim")
	op, _ := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if op.Mode != "clear" || op.State != "pending" {
		t.Fatalf("new request overwritten: %+v", op)
	}
}

func TestCacheCancellationRetainsIndexAndResumableIntent(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 20)
	file := cacheFile(t, f, "cancelled", 60, true)
	ctx, cancel := context.WithCancel(context.Background())
	h := &cacheFailureHub{storageHub: f.hub, cancel: true, hook: cancel}
	f.c.hub = h
	if err := f.st.RequestCacheOperations(f.cached.ID, []uint{f.node.ID}, "trim", true); err != nil {
		t.Fatal(err)
	}
	if err := f.c.reconcileStorageCache(ctx, f.cached.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ModelFileByID(file.ID); err != nil {
		t.Fatal("cancelled RPC removed index")
	}
	op, _ := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if op.State != "pending" {
		t.Fatalf("cancelled=%+v", op)
	}
}

func TestCachePlanInvalidatedByNewQuotaBeforeNextDeletion(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 20)
	cacheFile(t, f, "first", 60, true)
	second := cacheFile(t, f, "second", 60, true)
	h := &cacheFailureHub{storageHub: f.hub}
	f.c.hub = h
	h.hook = func() {
		limit := int64(1000)
		if _, err := f.st.UpdateStorage(f.cached.ID, store.StoragePatch{CacheMaxBytes: &limit}); err != nil {
			t.Fatal(err)
		}
	}
	runCache(t, f, "trim")
	if _, err := f.st.ModelFileByID(second.ID); err != nil {
		t.Fatal("stale plan deleted second file after quota increase")
	}
	if len(f.hub.calls) != 1 {
		t.Fatalf("calls=%v", f.hub.calls)
	}
}

func TestCacheTrimDoesNotSpendTheSameDiskSnapshotTwice(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 0)
	if err := f.st.UpdateNodeStatus(f.node.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Kind: proto.PathKindStorageCache, StorageID: f.cached.ID, Path: f.cached.CachePath, TotalBytes: 1000, AvailableBytes: 0}}}); err != nil {
		t.Fatal(err)
	}
	first := cacheFile(t, f, "first", 100, true)
	second := cacheFile(t, f, "second", 100, true)
	runCache(t, f, "trim")
	if _, err := f.st.ModelFileByID(first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first not removed: %v", err)
	}
	// A fresh controller has no in-memory credit. Durable deletion facts still
	// prevent consuming the same disk report again after restart.
	f.c = New(f.st, &cacheFailureHub{storageHub: f.hub}, nil)
	if err := f.st.UpdateNodeStatus(f.node.UUID, proto.NodeStatus{}); err != nil {
		t.Fatal(err)
	}
	runCache(t, f, "trim")
	if _, err := f.st.ModelFileByID(second.ID); err != nil {
		t.Fatal("second removed using already satisfied stale disk reserve")
	}
	if len(f.hub.calls) != 1 {
		t.Fatalf("calls=%v", f.hub.calls)
	}
	// An actual new statfs observation reporting continued pressure permits a
	// fresh reserve decision even when its numeric values happen to match.
	if err := f.st.UpdateNodeStatus(f.node.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Kind: proto.PathKindStorageCache, StorageID: f.cached.ID, Path: f.cached.CachePath, TotalBytes: 1000, AvailableBytes: 0}}}); err != nil {
		t.Fatal(err)
	}
	runCache(t, f, "trim")
	if _, err := f.st.ModelFileByID(second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fresh pressure not reconciled: %v", err)
	}
}

func TestCacheClearConsumesIntentButRecordsProtectedRemainder(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 1000)
	cacheFile(t, f, "safe", 60, true)
	cacheFile(t, f, "unbacked", 60, false)
	runCache(t, f, "clear")
	op, err := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if err != nil || op.State != "completed" || op.Error != "protected_entries_remain" {
		t.Fatalf("clear=%+v err=%v", op, err)
	}
}

func TestCacheEmptyRemovalResponseKeepsIndex(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 20)
	file := cacheFile(t, f, "empty-response", 60, true)
	f.c.hub = &cacheFailureHub{storageHub: f.hub, empty: true}
	runCache(t, f, "trim")
	if _, err := f.st.ModelFileByID(file.ID); err != nil {
		t.Fatal("unconfirmed deletion removed index")
	}
	op, _ := f.st.CacheOperation(f.cached.ID, f.node.ID)
	if op.State != "failed" {
		t.Fatalf("result=%+v", op)
	}
}

func TestCacheRPCObservesPersistedDiskFenceBeforeSideEffect(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 20)
	cacheFile(t, f, "fenced", 60, true)
	f.c.hub = &cacheFailureHub{storageHub: f.hub, fail: true, hook: func() {
		op, err := f.st.CacheOperation(f.cached.ID, f.node.ID)
		if err != nil || op.DiskInvalidatedAt == nil {
			t.Fatalf("RPC preceded durable disk fence: %+v %v", op, err)
		}
	}}
	runCache(t, f, "trim")
}
