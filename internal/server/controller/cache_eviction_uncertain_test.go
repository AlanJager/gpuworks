package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCacheUnconfirmedRemovalCannotBeReadOrRefilled(t *testing.T) {
	for _, mode := range []string{"async", "inline"} {
		for _, outcome := range []string{"partial_failure", "timeout"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				f := newStorageControllerFixture(t)
				prepareCache(t, f, 20)
				file := cacheFile(t, f, "candidate", 60, true)
				disk := t.TempDir()
				shard := filepath.Join(disk, "shard")
				if err := os.WriteFile(shard, []byte("weights"), 0600); err != nil {
					t.Fatal(err)
				}
				finishRemote := make(chan struct{})
				remoteDone := make(chan struct{})
				defer func() { close(finishRemote); <-remoteDone }()
				f.c.hub = &cacheEvictionContextHub{storageHub: f.hub, call: func(context.Context) (json.RawMessage, error) {
					if outcome == "timeout" {
						go func() { <-finishRemote; _ = os.Remove(shard); close(remoteDone) }()
						return nil, context.DeadlineExceeded
					}
					if err := os.Remove(shard); err != nil {
						t.Fatal(err)
					}
					close(remoteDone)
					return nil, errors.New("second shard: permission denied")
				}}
				if mode == "async" {
					runCache(t, f, "clear")
				} else if err := f.c.evictCacheRow(t.Context(), f.node, file.ID); err == nil {
					t.Fatal("expected failure")
				}
				after, err := f.st.ModelFileByID(file.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Ready() {
					t.Error("uncertain/partially deleted cache still ready")
				}
				src, err := f.st.ModelSourceOn(file.ModelID, f.node, store.SourceRules{Pinned: []uint{f.cached.ID}})
				if err != nil {
					t.Fatal(err)
				}
				if src.Action == store.SourceReady {
					t.Error("launch can still select deleting cache")
				}
				model, err := f.st.GetModel(file.ModelID)
				if err != nil {
					t.Fatal(err)
				}
				if task, ok := f.st.ModelFileTask(after, model); ok && task.DesiredState == "present" {
					t.Error("reconnect can refill unresolved deletion")
				}
				if _, err := f.st.PrepareModelCopy(store.FileRef{ModelID: file.ModelID, StorageID: file.StorageID, NodeID: file.NodeID}, "candidate"); err == nil {
					t.Error("replacement copy accepted before deletion confirmed")
				}
				for _, token := range []string{"", file.TransferID} {
					if err := f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), State: proto.MFPresent, TransferID: token}); err != nil {
						t.Fatal(err)
					}
				}
				after, err = f.st.ModelFileByID(file.ID)
				if err != nil || after.Ready() {
					t.Fatalf("late status resurrected deleting cache: %+v %v", after, err)
				}
			})
		}
	}
}

// Lost replies must reuse the persisted intent even after a policy change.
func TestCacheDeletionRetryKeepsIdentityAfterPolicyChange(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 20)
	file := cacheFile(t, f, "candidate", 60, true)
	f.c.hub = &cacheEvictionContextHub{storageHub: f.hub, call: func(context.Context) (json.RawMessage, error) { return nil, context.DeadlineExceeded }}
	runCache(t, f, "clear")
	deleting, err := f.st.ModelFileByID(file.ID)
	if err != nil || deleting.State != proto.MFDeleting || deleting.TransferID == "" {
		t.Fatalf("deletion intent=%+v %v", deleting, err)
	}
	// A new store consumer sees the durable fence, regardless of old in-memory pins.
	model, err := f.st.GetModel(file.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func() error{
		func() error {
			_, err := f.st.RegisterModelFile(store.FileRef{ModelID: file.ModelID, StorageID: file.StorageID, NodeID: file.NodeID}, file.Path, 60)
			return err
		},
		func() error { _, err := f.st.UpsertModelFile(file); return err },
	} {
		if err := change(); !errors.Is(err, store.ErrCacheDeletionPending) {
			t.Fatalf("unsafe replacement: %v", err)
		}
	}
	if err := f.st.SetModelFileState(file.ID, proto.MFPresent, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), State: proto.MFPresent, TransferID: deleting.TransferID}); err != nil {
		t.Fatal(err)
	}
	cap := int64(10000)
	if _, err := f.st.UpdateStorage(f.cached.ID, store.StoragePatch{CacheMaxBytes: &cap}); err != nil {
		t.Fatal(err)
	}
	f.c.hub = &cacheReceiptHub{storageHub: f.hub, check: func(params map[string]string) {
		if params["eviction_id"] != deleting.TransferID || params["path"] != deleting.Path {
			t.Errorf("retry changed identity: %+v", params)
		}
	}}
	runCache(t, f, "trim")
	if _, err := f.st.ModelFileByID(file.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("confirmed deletion survived relaxed cap: %v", err)
	}
	if task, ok := f.st.ModelFileTask(deleting, model); !ok || task.DesiredState != "absent" {
		t.Fatalf("deleting task=%+v %v", task, ok)
	}
}

type cacheReceiptHub struct {
	*storageHub
	check func(map[string]string)
}

func (h *cacheReceiptHub) CallNode(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
	p := params.(map[string]string)
	h.check(p)
	if method != proto.RPCCacheEvict {
		return nil, errors.New("expected idempotent cache RPC")
	}
	return json.Marshal(map[string]any{"path": p["path"], "eviction_id": p["eviction_id"], "removed": true})
}

func TestCacheDeletionRecoveryDoesNotOverEvictHealthyFiles(t *testing.T) {
	f := newStorageControllerFixture(t)
	prepareCache(t, f, 100)
	old := cacheFile(t, f, "old", 100, true)
	f.c.hub = &cacheEvictionContextHub{storageHub: f.hub, call: func(context.Context) (json.RawMessage, error) { return nil, context.DeadlineExceeded }}
	runCache(t, f, "clear")
	healthy := cacheFile(t, f, "healthy", 100, true)
	f.c.hub = &cacheFailureHub{storageHub: f.hub}
	runCache(t, f, "trim")
	// The worker's second pass must re-read the post-deletion capacity.
	if err := f.c.reconcileStorageCache(t.Context(), f.cached.ID, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ModelFileByID(old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending removal not completed: %v", err)
	}
	if row, err := f.st.ModelFileByID(healthy.ID); err != nil || !row.Ready() {
		t.Fatalf("healthy file over-evicted: %+v %v", row, err)
	}
}
