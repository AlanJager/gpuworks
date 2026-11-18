package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelDownloadFailureCodesReachHTTPAndClearOnRetry(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, code := range []string{"MODEL_DOWNLOAD_NETWORK_EXHAUSTED", "MODEL_DOWNLOAD_SOURCE_NOT_FOUND", "MODEL_DOWNLOAD_SOURCE_ACCESS_DENIED"} {
			t.Run(fmt.Sprintf("shared=%t/%s", shared, code), func(t *testing.T) {
				r := newTokenRig(t, func(d *Deps) { d.Online = func(string) bool { return true } })
				t.Cleanup(func() { _ = r.st.Close() })
				node, err := r.st.RegisterNode("source-errors", store.HashToken("code"), "source-errors")
				if err != nil {
					t.Fatal(err)
				}
				if err := r.st.DB().Model(node).Update("state", "online").Error; err != nil {
					t.Fatal(err)
				}
				model, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/model", Revision: "abcdef1234567890abcdef1234567890abcdef12"})
				if err != nil {
					t.Fatal(err)
				}
				ref := store.FileRef{ModelID: model.ID, NodeID: node.ID, StorageID: r.st.DefaultLocalStorageID()}
				listKey := "nodes"
				if shared {
					storage, err := r.st.CreateStorage(&store.Storage{Name: "nfs-errors", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/mnt/models", Export: "nfs.example:/models"}, nil)
					if err != nil {
						t.Fatal(err)
					}
					ref.NodeID, ref.StorageID, listKey = 0, storage.ID, "storages"
					if err := r.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: proto.StorageStateReady, Path: "/mnt/models", Writable: true}}}); err != nil {
						t.Fatal(err)
					}
				}
				row, err := r.st.PrepareModelDownload(ref, "org/model")
				if err != nil {
					t.Fatal(err)
				}
				if shared {
					if err := r.st.SetModelFileWriter(row.ID, node.ID); err != nil {
						t.Fatal(err)
					}
				}
				wire := fmt.Sprintf(`{"key":%q,"state":"failed","transfer_id":%q,"transfer_error_code":%q,"acquisition":"download","message":"provider evidence"}`, store.ModelFileKey(row.ID), row.TransferID, code)
				var status proto.ModelFileStatus
				if err := json.Unmarshal([]byte(wire), &status); err != nil {
					t.Fatal(err)
				}
				if err := r.st.UpdateModelFileStatus(node.ID, status); err != nil {
					t.Fatal(err)
				}
				get := func() map[string]any {
					t.Helper()
					rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/models/%d", model.ID), "")
					if rec.Code != http.StatusOK {
						t.Fatalf("get=%d %s", rec.Code, rec.Body)
					}
					var item map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
						t.Fatal(err)
					}
					rows, ok := item[listKey].([]any)
					if !ok || len(rows) != 1 {
						t.Fatalf("presence=%s", rec.Body)
					}
					return rows[0].(map[string]any)
				}
				if got := get(); got["transfer_error_code"] != code || got["state"] != "failed" {
					t.Fatalf("HTTP lost error classification: %+v", got)
				}
				rec := r.do(r.session, http.MethodPost, fmt.Sprintf("/api/v1/models/%d/download", model.ID), fmt.Sprintf(`{"storage_id":%d,"node_uuid":%q}`, ref.StorageID, node.UUID))
				if rec.Code != http.StatusAccepted {
					t.Fatalf("retry=%d %s", rec.Code, rec.Body)
				}
				if got := get(); got["transfer_error_code"] != nil {
					t.Fatalf("retry retained stale outcome: %+v", got)
				}
			})
		}
	}
}
