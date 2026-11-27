package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A download the agent says is waiting to ask its address again carries that
// retry on the model page and in a deployment's preparation, as a typed field
// with its exact wire names; the next frame that moves bytes ends it, and the
// row is never written for it (GWKS-526).
func TestADownloadsRetryReachesTheModelPageAndThePreparation(t *testing.T) {
	st := modelEvictionStore(t, "fetch-retry")
	model, node := modelEvictionFixture(t, st)
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	file := &store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFDownloading, Acquisition: proto.ModelAcquireDownload, TransferID: "dl-1", SizeBytes: 100, Downloaded: 99, Message: "downloading from modelscope.cn"}
	if _, err := st.UpsertModelFile(file); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "retrying", ModelID: model.ID, NodeID: node.ID})
	if err != nil {
		t.Fatal(err)
	}
	prep := newTestPreparation()
	handler := &apiHandlers{d: Deps{Store: st, Preparation: prep}}
	frame := proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), State: proto.MFDownloading, TransferID: "dl-1", Acquisition: proto.ModelAcquireDownload, BytesDone: 99, BytesTotal: 100, Message: "downloading from modelscope.cn",
		Retry: &proto.ModelFetchRetry{Attempt: 4, Attempts: 16, WaitSeconds: 8, Cause: "read: connection reset by peer", File: "assets/dsv4_performance.png"}}
	prep.record(node.ID, frame)
	if err := st.UpdateModelFileStatus(node.ID, frame); err != nil {
		t.Fatal(err)
	}

	const wire = `{"attempt":4,"attempts":16,"wait_seconds":8,"cause":"read: connection reset by peer","file":"assets/dsv4_performance.png"}`
	retryOf := func(v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		return string(row["retry"])
	}
	page := func() dto.ModelNodePresence {
		t.Helper()
		var d dto.Model
		handler.fillModelPresence(&d, model.ID)
		if len(d.Nodes) != 1 {
			t.Fatalf("nodes=%+v", d.Nodes)
		}
		return d.Nodes[0]
	}
	preparation := func() dto.ModelNodePresence {
		t.Helper()
		shown := handler.deploymentDTO(dep, false).ModelProgress
		if len(shown) != 1 {
			t.Fatalf("preparation=%+v", shown)
		}
		return shown[0]
	}
	if got := retryOf(page()); got != wire {
		t.Fatalf("model page retry=%s, want %s", got, wire)
	}
	if got := retryOf(preparation()); got != wire {
		t.Fatalf("preparation retry=%s, want %s", got, wire)
	}
	if row, err := st.ModelFileByID(file.ID); err != nil || row.Message != "downloading from modelscope.cn" {
		t.Fatalf("the row took the retry: %+v err=%v", row, err)
	}

	// A shared row and a throttled wait carry the field the same way.
	storage, err := st.CreateStorage(&store.Storage{Name: "shared-retry", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/shared", Export: "fixture:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	shared := proto.ModelFileStatus{Key: "", State: proto.MFDownloading, TransferID: "dl-shared", Acquisition: proto.ModelAcquireDownload,
		Retry: &proto.ModelFetchRetry{Attempt: 2, Attempts: 16, WaitSeconds: 110, Throttled: true}}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, WriterNodeID: node.ID, State: proto.MFDownloading, Acquisition: proto.ModelAcquireDownload, TransferID: "dl-shared"}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ModelFilesForModel(model.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sharedRow *store.ModelFile
	for i := range rows {
		if rows[i].TransferID == "dl-shared" {
			sharedRow = &rows[i]
		}
	}
	if sharedRow == nil {
		t.Fatal("the shared row is missing")
	}
	shared.Key = store.ModelFileKey(sharedRow.ID)
	prep.record(node.ID, shared)
	var sharedPage dto.Model
	handler.fillModelPresence(&sharedPage, model.ID)
	if len(sharedPage.Storages) != 1 {
		t.Fatalf("storages=%+v", sharedPage.Storages)
	}
	if got := retryOf(sharedPage.Storages[0]); got != `{"attempt":2,"attempts":16,"wait_seconds":110,"throttled":true}` {
		t.Fatalf("shared retry=%s", got)
	}

	moving := frame
	moving.Retry, moving.BytesDone = nil, 100
	prep.record(node.ID, moving)
	if got := retryOf(page()); got != "" {
		t.Fatalf("model page kept the retry after bytes moved: %s", got)
	}
	list, err := deployment.NewListRows(st, prep, []store.Deployment{*dep}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if shown, _ := deployment.ModelProgress(list, dep); len(shown) != 1 || shown[0].Retry != nil {
		t.Fatalf("the list kept the retry: %+v", shown)
	}
}
