package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageDeleteOtherSourceReader(t *testing.T) {
	f := newStorageDeleteFixture(t)
	other := f.storage("other")
	m := f.model("replicated-reader", f.target, other)
	file, err := f.st.ModelFileAt(store.FileRef{ModelID: m.ID, StorageID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "reader", ServedName: "reader", ModelID: m.ID,
		State: store.DeployActive, SaveToStorageID: f.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateInstances([]*store.Instance{{UUID: "other-reader", DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstRunning}},
		[][]store.InstanceModelFile{{{ModelID: m.ID, ModelFileID: file.ID, HostPath: filepath.Join(other.Path, file.Path)}}}); err != nil {
		t.Fatal(err)
	}
	f.expectStorageUsers(f.target)
	f.expectStorageUsers(other, dep.ID)
	f.delete(http.StatusNoContent)
	f.expectStorage(f.target, http.StatusNotFound)
	f.expectModel(m, true)
	f.expectFileStorages(m, other.ID)
	if err := f.st.DB().First(dep, dep.ID).Error; err != nil || dep.SaveToStorageID != 0 {
		t.Fatalf("download destination after delete = %+v, %v", dep, err)
	}
	var source store.InstanceModelFile
	if err := f.st.DB().First(&source).Error; err != nil || source.ModelFileID != file.ID {
		t.Fatalf("other storage's read source changed: %+v, %v", source, err)
	}
}

func TestStorageDeleteCacheRegistrations(t *testing.T) {
	f := newStorageDeleteFixture(t)
	if err := f.st.DB().Model(f.target).Update("type", proto.StorageNFSCache).Error; err != nil {
		t.Fatal(err)
	}
	m := f.model("cached", f.target)
	cachePath := filepath.Join(t.TempDir(), "cached-model")
	if err := os.WriteFile(cachePath, []byte("cached weights"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: m.ID, StorageID: f.target.ID, NodeID: f.node.ID,
		Path: cachePath, State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	f.delete(http.StatusNoContent)
	f.expectModel(m, false)
	files, err := f.st.ModelFilesInStorage(f.target.ID)
	if err != nil || len(files) != 0 {
		t.Fatalf("remaining authoritative/cache registrations = %+v, %v", files, err)
	}
	if got, err := os.ReadFile(cachePath); err != nil || string(got) != "cached weights" {
		t.Fatalf("physical cache changed: %q, %v", got, err)
	}
}

func TestStorageDeleteReadFailurePreservesRecords(t *testing.T) {
	f := newStorageDeleteFixture(t)
	m := f.model("only", f.target)
	if err := f.st.DB().Exec("DROP TABLE instance_model_files").Error; err != nil {
		t.Fatal(err)
	}
	f.delete(http.StatusInternalServerError)
	f.expectStorage(f.target, http.StatusOK)
	f.expectModel(m, true)
	f.expectFileStorages(m, f.target.ID)
}

func TestStorageDeletePendingCacheDeletionPreservesRecords(t *testing.T) {
	f := newStorageDeleteFixture(t)
	if err := f.st.DB().Model(f.target).Update("type", proto.StorageNFSCache).Error; err != nil {
		t.Fatal(err)
	}
	m := f.model("pending-cache", f.target)
	cachePath := filepath.Join(t.TempDir(), "weights")
	if err := os.WriteFile(cachePath, []byte("cached weights"), 0600); err != nil {
		t.Fatal(err)
	}
	cached, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: m.ID, StorageID: f.target.ID,
		NodeID: f.node.ID, Path: cachePath, State: proto.MFDeleting, TransferID: "unconfirmed-removal"})
	if err != nil {
		t.Fatal(err)
	}
	f.delete(http.StatusConflict)
	f.expectStorage(f.target, http.StatusOK)
	f.expectModel(m, true)
	if row, err := f.st.ModelFileByID(cached.ID); err != nil || row.State != proto.MFDeleting || row.TransferID != cached.TransferID {
		t.Fatalf("unconfirmed cache deletion changed: %+v, %v", row, err)
	}
	if data, err := os.ReadFile(cachePath); err != nil || string(data) != "cached weights" {
		t.Fatalf("unconfirmed cache file changed: %q, %v", data, err)
	}
}

func TestStorageDeleteCleansCacheOperations(t *testing.T) {
	f := newStorageDeleteFixture(t)
	other := f.storage("other")
	for _, location := range []*store.Storage{f.target, other} {
		if err := f.st.DB().Create(&store.StorageCacheOperation{StorageID: location.ID, NodeID: f.node.ID,
			Generation: 1, Mode: "trim", State: "completed"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.delete(http.StatusNoContent)
	if rows, err := f.st.CacheOperations(f.target.ID); err != nil || len(rows) != 0 {
		t.Fatalf("deleted storage retained cache operations: %+v, %v", rows, err)
	}
	if rows, err := f.st.CacheOperations(other.ID); err != nil || len(rows) != 1 {
		t.Fatalf("other storage's cache operations changed: %+v, %v", rows, err)
	}
}

func TestStorageDeleteFileFailurePreservesStorageTransaction(t *testing.T) {
	f := newStorageDeleteFixture(t)
	first := f.model("first", f.target)
	second := f.model("second", f.target)
	file, err := f.st.ModelFileAt(store.FileRef{ModelID: second.ID, StorageID: f.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.failDelete("model_files", file.ID)
	f.delete(http.StatusInternalServerError)
	// A failed registration deletion must preserve the entire operation.
	f.expectStorage(f.target, http.StatusOK)
	f.expectModel(first, true)
	f.expectModel(second, true)
	f.expectFileStorages(first, f.target.ID)
	f.expectFileStorages(second, f.target.ID)
	if row, err := f.st.ModelFileByID(file.ID); err != nil || row.StorageID != f.target.ID {
		t.Fatalf("failed operation changed the registration: %+v, %v", row, err)
	}
}

func TestStorageDeleteNodeErrorsLogAfterCommit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     string
		rpcError  error
		syncError error
	}{
		{name: "rpc_failure", rpcError: errors.New("agent disconnected")},
		{name: "empty_reply"},
		{name: "malformed_reply", reply: `{"peers":`},
		{name: "sync_failure", reply: `{"peers":[]}`, syncError: errors.New("sync disconnected")},
		{name: "rpc_and_sync_failure", rpcError: errors.New("heartbeat permission denied"), syncError: errors.New("sync disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, syncs int
			var logs bytes.Buffer
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.Log = slog.New(slog.NewTextHandler(&logs, nil))
				d.CallNode = func(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
					calls++
					p, ok := params.(proto.StoragePeersParams)
					if method != proto.RPCStoragePeers || !ok || !p.Remove || p.StorageID == 0 || p.ControlPlaneID == "" {
						t.Fatalf("unexpected storage cleanup: %s %+v", method, params)
					}
					return json.RawMessage(tc.reply), tc.rpcError
				}
				d.NodeSync = nodeSyncCallbacks{full: func(string) error { syncs++; return tc.syncError }}
			})
			m := f.model("only", f.target)
			body := f.request(http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", f.target.ID), http.StatusNoContent)
			if len(body) != 0 {
				t.Fatalf("successful delete must have an empty body: %s", body)
			}
			for _, text := range []string{"level=WARN", "node cleanup failed", fmt.Sprintf("storage_id=%d", f.target.ID), f.node.UUID} {
				if !strings.Contains(logs.String(), text) {
					t.Errorf("cleanup log lacks %q: %s", text, logs.String())
				}
			}
			for _, cause := range []error{tc.rpcError, tc.syncError} {
				if cause != nil && !strings.Contains(logs.String(), cause.Error()) {
					t.Errorf("cleanup log lost cause %q: %s", cause, logs.String())
				}
			}
			f.expectStorage(f.target, http.StatusNotFound)
			f.expectModel(m, false)
			if calls != 1 || syncs != 1 {
				t.Fatalf("cleanup calls/syncs = %d/%d, want one attempt each", calls, syncs)
			}
			f.delete(http.StatusNotFound)
		})
	}
}

func TestStorageDeleteModelFailureDoesNotReachNodes(t *testing.T) {
	var calls, syncs int
	f := newStorageDeleteFixture(t, func(d *Deps) {
		d.CallNode = func(_ context.Context, _ string, method string, _ any) (json.RawMessage, error) {
			calls++
			if method != proto.RPCStoragePeers {
				t.Fatalf("unexpected file operation %q", method)
			}
			return json.RawMessage(`{"peers":[]}`), nil
		}
		d.NodeSync = nodeSyncCallbacks{full: func(string) error { syncs++; return nil }}
	})
	m := f.model("only", f.target)
	f.failDelete("models", m.ID)
	f.delete(http.StatusInternalServerError)
	f.expectStorage(f.target, http.StatusOK)
	f.expectModel(m, true)
	f.expectFileStorages(m, f.target.ID)
	if calls != 0 || syncs != 0 {
		t.Fatalf("uncommitted storage removal reached nodes: calls/syncs %d/%d", calls, syncs)
	}
}
