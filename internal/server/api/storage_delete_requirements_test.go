package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ZSTAC-89423: deleting storage changes platform records, never model files.
func TestStorageDeleteRequirements(t *testing.T) {
	t.Run("only_copy_removes_model_entry", func(t *testing.T) {
		f := newStorageDeleteFixture(t)
		m := f.model("only", f.target)
		f.delete(http.StatusNoContent)
		f.expectStorage(f.target, http.StatusNotFound)
		f.expectModel(m, false)
	})
	t.Run("another_copy_preserves_model_and_other_storage", func(t *testing.T) {
		f := newStorageDeleteFixture(t)
		other := f.storage("other")
		m := f.model("replicated", f.target, other)
		f.delete(http.StatusNoContent)
		f.expectStorage(f.target, http.StatusNotFound)
		f.expectStorage(other, http.StatusOK)
		f.expectModel(m, true)
		f.expectFileStorages(m, other.ID)
	})
	t.Run("multiple_models_are_decided_individually", func(t *testing.T) {
		f := newStorageDeleteFixture(t)
		other := f.storage("other")
		only := f.model("only", f.target)
		shared := f.model("shared", f.target, other)
		unrelated := f.model("unrelated", other)
		f.delete(http.StatusNoContent)
		f.expectStorage(f.target, http.StatusNotFound)
		f.expectModel(only, false)
		f.expectModel(shared, true)
		f.expectFileStorages(shared, other.ID)
		f.expectModel(unrelated, true)
		f.expectFileStorages(unrelated, other.ID)
	})
	for _, state := range []string{proto.InstRunning, proto.InstStopped, proto.InstFailed} {
		t.Run("known_reader_"+state+"_blocks_delete", func(t *testing.T) {
			f := newStorageDeleteFixture(t)
			m := f.model("used", f.target)
			files, err := f.st.ModelFilesForModel(m.ID)
			if err != nil || len(files) != 1 {
				t.Fatalf("prepare model files: %+v, %v", files, err)
			}
			dep, err := f.st.CreateDeployment(&store.Deployment{Name: "reader", ServedName: "reader", ModelID: m.ID, State: store.DeployActive})
			if err != nil {
				t.Fatal(err)
			}
			err = f.st.CreateInstances([]*store.Instance{{UUID: "known-reader", DeploymentID: dep.ID, NodeID: f.node.ID, State: state}},
				[][]store.InstanceModelFile{{{ModelID: m.ID, ModelFileID: files[0].ID, HostPath: filepath.Join(f.target.Path, files[0].Path)}}})
			if err != nil {
				t.Fatal(err)
			}
			f.expectStorageUsers(f.target, dep.ID)
			f.delete(http.StatusConflict)
			f.expectStorage(f.target, http.StatusOK)
			f.expectModel(m, true)
			f.expectFileStorages(m, f.target.ID)
		})
	}
	t.Run("explicit_storage_requirement_blocks_delete", func(t *testing.T) {
		f := newStorageDeleteFixture(t)
		m := f.model("pinned", f.target)
		dep, err := f.st.CreateDeploymentWithRequirements(&store.Deployment{Name: "pinned", ServedName: "pinned", ModelID: m.ID},
			nodelabel.Requirements{nodelabel.KeyStorage: {fmt.Sprint(f.target.ID)}})
		if err != nil {
			t.Fatal(err)
		}
		f.expectStorageUsers(f.target, dep.ID)
		f.delete(http.StatusConflict)
		f.expectStorage(f.target, http.StatusOK)
		f.expectModel(m, true)
		f.expectFileStorages(m, f.target.ID)
	})
	t.Run("unexpected_storage_error_returns_failure", func(t *testing.T) {
		f := newStorageDeleteFixture(t)
		f.model("only", f.target)
		f.failDelete("storages", f.target.ID)
		f.delete(http.StatusInternalServerError)
	})
	t.Run("cleanup_error_does_not_restore_deleted_storage", func(t *testing.T) {
		f := newStorageDeleteFixture(t)
		m := f.model("cleanup-fails", f.target)
		f.failDelete("models", m.ID)
		f.delete(http.StatusInternalServerError)
		f.expectStorage(f.target, http.StatusNotFound)
		f.expectModel(m, true)
		f.expectFileStorages(m)
		// A second manual DELETE has no success contract in this task.
	})
}

type storageDeleteFixture struct {
	t        *testing.T
	st       *store.Store
	router   http.Handler
	token    string
	node     *store.Node
	target   *store.Storage
	statuses []proto.StorageStatus
}

func newStorageDeleteFixture(t *testing.T, configure ...func(*Deps)) *storageDeleteFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("storage-reader", store.HashToken("test-token"), "storage-reader")
	if err != nil {
		t.Fatal(err)
	}
	signer, token := roleTestToken(t, st, "admin")
	f := &storageDeleteFixture{t: t, st: st, token: token, node: node}
	d := Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return true },
		CallNode: func(_ context.Context, _ string, method string, _ any) (json.RawMessage, error) {
			if method != proto.RPCStoragePeers {
				t.Errorf("storage deletion must not request model file removal: RPC %q", method)
			}
			return json.RawMessage(`{"peers":[]}`), nil
		}}
	for _, setup := range configure {
		setup(&d)
	}
	f.router = Router(d)
	f.target = f.storage("target")
	return f
}

func (f *storageDeleteFixture) storage(name string) *store.Storage {
	f.t.Helper()
	row, err := f.st.CreateStorage(&store.Storage{Name: name, Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: f.t.TempDir(), Export: "nfs.test:/" + name}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.statuses = append(f.statuses, proto.StorageStatus{ID: row.ID, State: proto.StorageStateReady, Path: row.Path, Writable: true})
	err = f.st.ApplyStorageStatus(f.node.ID, proto.StorageStatusReport{Storages: f.statuses})
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

func (f *storageDeleteFixture) model(name string, locations ...*store.Storage) *store.Model {
	f.t.Helper()
	m, err := f.st.UpsertModel(&store.Model{Name: name, Source: "local", SourceID: name, Revision: "local"})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, location := range locations {
		path := filepath.Join(location.Path, name)
		content := []byte("preserved model: " + name)
		if err := os.WriteFile(path, content, 0600); err != nil {
			f.t.Fatal(err)
		}
		// Runs even after a failed HTTP assertion, so file preservation is
		// checked independently in success, refusal and error scenarios.
		f.t.Cleanup(func() {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(content) {
				f.t.Errorf("physical model changed: %s: %q, %v", path, got, err)
			}
		})
		if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: m.ID, StorageID: location.ID, Path: name, State: proto.MFPresent}); err != nil {
			f.t.Fatal(err)
		}
	}
	return m
}

func (f *storageDeleteFixture) request(method, path string, want int) []byte {
	f.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != want {
		f.t.Errorf("%s %s = %d %s; want %d", method, path, rec.Code, rec.Body.String(), want)
	}
	return rec.Body.Bytes()
}

func (f *storageDeleteFixture) delete(want int) {
	f.t.Helper()
	f.request(http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", f.target.ID), want)
}

func (f *storageDeleteFixture) expectStorage(row *store.Storage, want int) {
	f.t.Helper()
	f.request(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d", row.ID), want)
}

func (f *storageDeleteFixture) expectStorageUsers(row *store.Storage, want ...uint) {
	f.t.Helper()
	var detail dto.Storage
	if err := json.Unmarshal(f.request(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d", row.ID), http.StatusOK), &detail); err != nil {
		f.t.Fatal(err)
	}
	if !slices.Equal(detail.DeploymentIDs, want) {
		f.t.Errorf("storage detail users = %v, want %v", detail.DeploymentIDs, want)
	}
	var list struct{ Items []dto.Storage }
	if err := json.Unmarshal(f.request(http.MethodGet, "/api/v1/storages", http.StatusOK), &list); err != nil {
		f.t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.ID == row.ID {
			if !slices.Equal(item.DeploymentIDs, want) {
				f.t.Errorf("storage list users = %v, want %v", item.DeploymentIDs, want)
			}
			return
		}
	}
	f.t.Fatalf("storage %d missing from list", row.ID)
}

func (f *storageDeleteFixture) expectModel(model *store.Model, present bool) {
	f.t.Helper()
	var list struct {
		Items []struct{ ID uint }
	}
	if err := json.Unmarshal(f.request(http.MethodGet, "/api/v1/models", http.StatusOK), &list); err != nil {
		f.t.Fatal(err)
	}
	found := false
	for _, item := range list.Items {
		found = found || item.ID == model.ID
	}
	if found != present {
		f.t.Errorf("model %q visible = %v; want %v", model.Name, found, present)
	}
}

func (f *storageDeleteFixture) expectFileStorages(model *store.Model, want ...uint) {
	f.t.Helper()
	var view dto.Model
	if err := json.Unmarshal(f.request(http.MethodGet, fmt.Sprintf("/api/v1/models/%d", model.ID), http.StatusOK), &view); err != nil {
		f.t.Fatal(err)
	}
	got := make(map[uint]bool)
	for _, file := range view.Storages {
		got[file.StorageID] = true
	}
	if len(view.Storages) != len(want) {
		f.t.Errorf("model %q storage copies = %+v; want storage IDs %v", model.Name, view.Storages, want)
	}
	for _, id := range want {
		if !got[id] {
			f.t.Errorf("model %q lost storage %d", model.Name, id)
		}
	}
}

func (f *storageDeleteFixture) failDelete(table string, id uint) {
	f.t.Helper()
	// SQLite faults exercise the real HTTP error path without replacing
	// the deletion implementation with a mock.
	sql := fmt.Sprintf("CREATE TRIGGER reject_delete BEFORE DELETE ON %s WHEN OLD.id = %d BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END", table, id)
	if err := f.st.DB().Exec(sql).Error; err != nil {
		f.t.Fatal(err)
	}
}
