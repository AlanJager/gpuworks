package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A create that saves into a storage fetches nothing by itself for a local
// storage (each node gets the model when it starts, from a node of its sync
// group or by downloading it) and starts exactly one download through a
// writer node for a shared storage that does not hold the model yet. The
// per-deployment network choice of the earlier form is gone: a request that
// still carries one is accepted, and the task carries no route of its own, so
// the node's own network settings apply.
func TestCreateDeploymentSavingToAStorage(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			h, st := automaticPlacementFixture(t)
			modelID := automaticPlacementModel(t, st)
			if err := st.SetSetting("network", `{"https_proxy":"http://proxy.invalid:3128","rewrites":[{"from":"https://huggingface.co","to":"https://mirror.invalid"}]}`); err != nil {
				t.Fatal(err)
			}
			signer, err := auth.NewTokenSigner("download-network-route-secret", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
			if err != nil {
				t.Fatal(err)
			}
			h.d.AuthSigner = signer
			h.d.Online = func(string) bool { return true }
			storageID := st.DefaultLocalStorageID()
			if shared {
				storage, err := st.CreateStorage(&store.Storage{Name: "network-shared", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/mnt/models", Export: "nfs.example:/models"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				storageID = storage.ID
				node, err := st.NodeByUUID("auto-big")
				if err != nil {
					t.Fatal(err)
				}
				if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: proto.StorageStateReady, Path: "/mnt/models", Writable: true}}}); err != nil {
					t.Fatal(err)
				}
			}
			router := Router(h.d)
			// "network" and "kind" are keys of the earlier form; they are ignored.
			body := fmt.Sprintf(`{"name":"save-to","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","model_source":{"kind":"public","storage_id":%d,"provider":"huggingface","network":{"mode":"direct"}}}`, modelID, storageID)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create=%d %s", rec.Code, rec.Body)
			}
			node, err := st.NodeByUUID("auto-big")
			if err != nil {
				t.Fatal(err)
			}
			desired, err := st.DesiredStateForNode(node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !shared {
				if len(desired.ModelFiles) != 0 {
					t.Fatalf("a local storage needs nothing at create, got tasks %+v", desired.ModelFiles)
				}
				return
			}
			if len(desired.ModelFiles) != 1 {
				t.Fatalf("model files = %+v", desired.ModelFiles)
			}
			encoded, err := json.Marshal(desired.ModelFiles[0])
			if err != nil {
				t.Fatal(err)
			}
			var task map[string]any
			if err := json.Unmarshal(encoded, &task); err != nil {
				t.Fatal(err)
			}
			if task["acquisition"] != "download" || task["storage_id"] != float64(storageID) || task["source"] != "huggingface" {
				t.Fatalf("the shared download is not planned into the chosen storage, wire task = %s", encoded)
			}
			if task["network_explicit"] != nil || task["proxy"] != nil || task["endpoint"] != nil {
				t.Fatalf("the task carries a network route of its own, wire task = %s", encoded)
			}
			row, err := st.ModelFileAt(store.FileRef{ModelID: modelID, StorageID: storageID})
			if err != nil || row.WriterNodeID != node.ID {
				t.Fatalf("shared download row=%+v err=%v, want the online writer node %d", row, err, node.ID)
			}
		})
	}
}
