package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageDeleteModelReferenceWithoutInstances(t *testing.T) {
	for _, state := range store.DeploymentStates() {
		t.Run(state.String(), func(t *testing.T) {
			var calls, syncs int
			f := newStorageDeleteFixture(t, func(d *Deps) {
				d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
					calls++
					return json.RawMessage(`{"peers":[]}`), nil
				}
				d.NodeSync = nodeSyncCallbacks{full: func(string) error { syncs++; return nil }}
			})
			// The unreferenced model sorts first, but must not be removed
			// before the later model's reference rejects the whole request.
			unused := f.model("unused-first", f.target)
			used := f.model("used-second", f.target)
			var users []uint
			for _, name := range []string{"first-service", "second-service"} {
				dep, err := f.st.CreateDeployment(&store.Deployment{
					Name: name, ServedName: name, ModelID: used.ID, State: state, SaveToStorageID: f.target.ID,
				})
				if err != nil {
					t.Fatal(err)
				}
				users = append(users, dep.ID)
			}
			var instances int64
			if err := f.st.DB().Model(&store.Instance{}).Count(&instances).Error; err != nil || instances != 0 {
				t.Fatalf("fixture requires no instances: %d, %v", instances, err)
			}
			f.expectStorageUsers(f.target, users...)
			f.expectModelReferenceRefusal(users...)
			f.expectStorage(f.target, http.StatusOK)
			for _, m := range []*store.Model{unused, used} {
				f.expectModel(m, true)
				f.expectFileStorages(m, f.target.ID)
			}
			for _, id := range users {
				dep, err := f.st.GetDeployment(id)
				if err != nil || dep.ModelID != used.ID || dep.SaveToStorageID != f.target.ID || dep.State != state {
					t.Fatalf("refusal changed service configuration: %+v, %v", dep, err)
				}
			}
			if calls != 0 || syncs != 0 {
				t.Fatalf("refused delete reached nodes: calls=%d syncs=%d", calls, syncs)
			}
		})
	}
}

func TestStorageDeleteModelReferenceWithOtherSource(t *testing.T) {
	for _, tc := range []struct {
		state string
		keep  bool
	}{
		{proto.MFPresent, true}, {proto.MFRegistered, true},
		{proto.MFPending, true}, {proto.MFDownloading, true}, {proto.MFSyncing, true}, {proto.MFDeleting, true},
		{proto.MFMissing, false}, {proto.MFAbsent, false},
		{proto.MFFailed, false}, {proto.MFDownloadRequired, false}, {proto.MFInsufficientDisk, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := newStorageDeleteFixture(t)
			other := f.storage("other")
			locations := []*store.Storage{f.target}
			if proto.ModelFileStateReady(tc.state) {
				locations = append(locations, other)
			}
			m := f.model("referenced", locations...)
			file, err := f.st.UpsertModelFile(&store.ModelFile{
				ModelID: m.ID, StorageID: other.ID, Path: m.Name, State: tc.state,
			})
			if err != nil {
				t.Fatal(err)
			}
			dep, err := f.st.CreateDeployment(&store.Deployment{
				Name: "no-instance", ServedName: "no-instance", ModelID: m.ID, State: store.DeployActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.keep {
				f.expectStorageUsers(f.target)
				f.delete(http.StatusNoContent)
				f.expectStorage(f.target, http.StatusNotFound)
				f.expectModel(m, true)
				f.expectFileStorages(m, other.ID)
			} else {
				f.expectStorageUsers(f.target, dep.ID)
				f.expectModelReferenceRefusal(dep.ID)
				f.expectStorage(f.target, http.StatusOK)
				f.expectModel(m, true)
				f.expectFileStorages(m, f.target.ID, other.ID)
			}
			f.expectStorage(other, http.StatusOK)
			got, err := f.st.ModelFileByID(file.ID)
			if err != nil || got.State != tc.state || got.StorageID != other.ID {
				t.Fatalf("other source changed: %+v, %v", got, err)
			}
		})
	}
}

func TestStorageDeleteModelReferenceReadFailure(t *testing.T) {
	var calls int
	f := newStorageDeleteFixture(t, func(d *Deps) {
		d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
			calls++
			return json.RawMessage(`{"peers":[]}`), nil
		}
	})
	m := f.model("only", f.target)
	if err := f.st.DB().Exec("ALTER TABLE deployments RENAME TO unavailable_deployments").Error; err != nil {
		t.Fatal(err)
	}
	body := f.request(http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", f.target.ID), http.StatusInternalServerError)
	if err := f.st.DB().Exec("ALTER TABLE unavailable_deployments RENAME TO deployments").Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "no such table: deployments") {
		t.Errorf("lost reference query failure: %s", body)
	}
	f.expectStorage(f.target, http.StatusOK)
	f.expectModel(m, true)
	f.expectFileStorages(m, f.target.ID)
	if calls != 0 {
		t.Fatalf("reference query failure still withdrew heartbeat: %d calls", calls)
	}
}

func TestStorageDeleteOtherCopyDoesNotOverrideExplicitReader(t *testing.T) {
	f := newStorageDeleteFixture(t)
	other := f.storage("other")
	m := f.model("replicated", f.target, other)
	file, err := f.st.ModelFileAt(store.FileRef{ModelID: m.ID, StorageID: f.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "reader", ServedName: "reader", ModelID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateInstances([]*store.Instance{{UUID: "stopped-reader", DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstStopped}},
		[][]store.InstanceModelFile{{{ModelID: m.ID, ModelFileID: file.ID, HostPath: filepath.Join(f.target.Path, file.Path)}}}); err != nil {
		t.Fatal(err)
	}
	f.expectStorageUsers(f.target, dep.ID)
	f.delete(http.StatusConflict)
	f.expectStorage(f.target, http.StatusOK)
	f.expectModel(m, true)
	f.expectFileStorages(m, f.target.ID, other.ID)
}

func (f *storageDeleteFixture) expectModelReferenceRefusal(users ...uint) {
	f.t.Helper()
	body := f.request(http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", f.target.ID), http.StatusConflict)
	var response struct {
		Code   string
		Params struct {
			DeploymentIDs []uint   `json:"deployment_ids"`
			InstanceUUIDs []string `json:"instance_uuids"`
		}
	}
	if err := json.Unmarshal(body, &response); err != nil {
		f.t.Fatal(err)
	}
	if response.Code != ErrorStorageInUse || !slices.Equal(response.Params.DeploymentIDs, users) || len(response.Params.InstanceUUIDs) != 0 {
		f.t.Errorf("refusal must identify service references without inventing instances: %s", body)
	}
}
