package modeldownload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestModelCopyRejectsSameLocationAsBadRequest(t *testing.T) {
	env := testenv.New(t)
	st := env.Store()
	client := env.API(t)
	source, err := st.RegisterNode("copy-source", store.HashToken("source"), "copy-source")
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.RegisterNode("copy-target", store.HashToken("target"), "copy-target")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "copy-model", Name: "copy-model"})
	if err != nil {
		t.Fatal(err)
	}
	localID := st.DefaultLocalStorageID()
	shared, err := st.CreateStorage(&store.Storage{Name: "copy-shared", Type: proto.StorageSharedDir, Enabled: true, AllNodes: true, Path: "/mnt/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []*store.ModelFile{
		{ModelID: model.ID, StorageID: localID, NodeID: source.ID, Path: "/models/copy-model", State: proto.MFPresent},
		{ModelID: model.ID, StorageID: shared.ID, Path: "copy-model", State: proto.MFPresent},
	} {
		if _, err := st.UpsertModelFile(file); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/api/v1/models/%d", model.ID)
	snapshot := func() string {
		t.Helper()
		resp, err := client.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("get model: %d %s %v", resp.StatusCode, body, err)
		}
		return string(body)
	}
	before := snapshot()
	for _, tc := range []struct {
		name, body string
		status     int
		message    string
	}{
		{"local same node", fmt.Sprintf(`{"source":{"storage_id":%d,"node_uuid":%q},"target":{"storage_id":%d,"node_uuid":%q}}`, localID, source.UUID, localID, source.UUID), 400, "source and target are the same location"},
		{"shared same storage", fmt.Sprintf(`{"source":{"storage_id":%d},"target":{"storage_id":%d}}`, shared.ID, shared.ID), 400, "source and target are the same location"},
		{"shared different nodes still same file", fmt.Sprintf(`{"source":{"storage_id":%d,"node_uuid":%q},"target":{"storage_id":%d,"node_uuid":%q}}`, shared.ID, source.UUID, shared.ID, target.UUID), 400, "source and target are the same location"},
		{"different local node remains offline conflict", fmt.Sprintf(`{"source":{"storage_id":%d,"node_uuid":%q},"target":{"storage_id":%d,"node_uuid":%q}}`, localID, source.UUID, localID, target.UUID), 409, "target node is offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.PostJSON(path+"/copy", []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status || body["error"] != tc.message {
				t.Fatalf("status=%d body=%v; want %d %q", resp.StatusCode, body, tc.status, tc.message)
			}
			if tc.status == 400 && (body["code"] != "FIELD_INVALID" || body["params"].(map[string]any)["field"] != "target") {
				t.Fatalf("invalid target error contract = %v", body)
			}
			if after := snapshot(); after != before {
				t.Fatalf("rejected copy changed the model: before=%s after=%s", before, after)
			}
		})
	}
	unauthenticated := *client
	unauthenticated.Token = ""
	resp, err := unauthenticated.PostJSON(path+"/copy", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated copy status=%d", resp.StatusCode)
	}
}
