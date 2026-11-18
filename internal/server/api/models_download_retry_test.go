package api

import (
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Recovery is the existing download route, including authentication, desired
// state repush and a fresh transfer token. It must never rewrite model identity.
func TestFailedDownloadCanBeRetriedThroughAuthenticatedRoute(t *testing.T) {
	var pushed string
	r := newTokenRig(t, func(d *Deps) {
		d.NodeSync = nodeSyncCallbacks{full: func(uuid string) error { pushed = uuid; return nil }}
	})
	t.Cleanup(func() { _ = r.st.Close() })
	node, err := r.st.RegisterNode("download-retry", store.HashToken("retry"), "retry")
	if err != nil {
		t.Fatal(err)
	}
	model, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen2.5-1.5B", Revision: "8f8c715740f4d6e253f43b41939da5c5bb28e4c0"})
	if err != nil {
		t.Fatal(err)
	}
	ref := store.FileRef{ModelID: model.ID, StorageID: r.st.DefaultLocalStorageID(), NodeID: node.ID}
	row, err := r.st.PrepareModelDownload(ref, "Qwen/Qwen2.5-1.5B")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: store.ModelFileKey(row.ID), State: proto.MFFailed, TransferID: row.TransferID,
		Acquisition: proto.ModelAcquireDownload, Message: "list files: context deadline exceeded",
	}); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/models/%d/download", model.ID)
	body := fmt.Sprintf(`{"node_uuids":[%q],"storage_id":%d}`, node.UUID, ref.StorageID)
	if rec := r.do("", http.MethodPost, path, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated retry = %d", rec.Code)
	}
	if rec := r.do(r.session, http.MethodPost, path, body); rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body)
	}
	got, err := r.st.ModelFileAt(ref)
	if err != nil || got.State != proto.MFPending || got.TransferID == row.TransferID || got.TransferID == "" || pushed != node.UUID {
		t.Fatalf("retry did not schedule a fresh transfer: %+v push=%q err=%v", got, pushed, err)
	}
	desired, err := r.st.DesiredStateForNode(node.ID)
	if err != nil || len(desired.ModelFiles) != 1 || desired.ModelFiles[0].TransferID != got.TransferID || desired.ModelFiles[0].Revision != model.Revision {
		t.Fatalf("retry desired state = %+v err=%v", desired.ModelFiles, err)
	}
	unchanged, err := r.st.GetModel(model.ID)
	if err != nil || unchanged.Source != model.Source || unchanged.SourceID != model.SourceID || unchanged.Revision != model.Revision {
		t.Fatalf("retry rewrote model identity: %+v err=%v", unchanged, err)
	}
	// Late failure from the old attempt cannot fail the explicitly retried one.
	if err := r.st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(row.ID), State: proto.MFFailed, TransferID: row.TransferID}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.st.ModelFileAt(ref)
	if after.State != proto.MFPending || after.TransferID != got.TransferID {
		t.Fatalf("stale failure replaced retry: %+v", after)
	}
}
