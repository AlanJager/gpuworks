package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment's automatic peer acquisition enters through reconcile, rather
// than the explicit transfer API. It must reserve its own revision directory
// before sending a copy, even when the old owner reported an absolute path.
func TestDeploymentPeerAcquisitionSeparatesCompletedDefaultRevision(t *testing.T) {
	_, st, dep, target, holder, _, _ := imageCopyFixture(t, "revision_peer_acquisition")
	model, err := st.GetModel(dep.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Where("model_id = ? AND node_id = ?", model.ID, target.ID).Delete(&store.ModelFile{}).Error; err != nil {
		t.Fatal(err)
	}
	defaultModel, err := st.UpsertModel(&store.Model{Source: model.Source, SourceID: model.SourceID})
	if err != nil || defaultModel.ID == model.ID {
		t.Fatalf("default revision=%+v err=%v", defaultModel, err)
	}
	preferred := store.ModelArtifactPath(model)
	old, err := st.PrepareModelDownload(store.FileRef{ModelID: defaultModel.ID, NodeID: target.ID}, preferred)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(target.ID, proto.ModelFileStatus{
		Key: store.ModelFileKey(old.ID), State: proto.MFPresent,
		Path: filepath.Join("/var/lib/gpuworks/models", preferred), TransferID: old.TransferID,
		BytesTotal: 12, BytesDone: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: holder.ID,
		State: proto.MFPresent, Path: "/models/explicit-main", SizeBytes: 12, Downloaded: 12,
	}); err != nil {
		t.Fatal(err)
	}
	for i, node := range []*store.Node{target, holder} {
		if err := st.DB().Model(node).Update("network_inventory", managementInventoryJSON(t, fmt.Sprintf("10.0.0.%d", i+1))).Error; err != nil {
			t.Fatal(err)
		}
	}
	hub := &modelSyncHub{online: map[string]bool{target.UUID: true, holder.UUID: true}, called: make(chan struct{})}
	c := New(st, hub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("deployment reconcile did not send a peer acquisition")
	}
	row, err := st.ModelFileByModelNode(model.ID, target.ID)
	want := fmt.Sprintf("%s-model-%d", preferred, model.ID)
	if err != nil || row.Path != want || row.State != proto.MFSyncing || row.DownloadDeploymentID != dep.ID {
		t.Fatalf("persisted target=%+v want path=%q err=%v", row, want, err)
	}
	hub.mu.Lock()
	req := hub.requests[0]
	hub.mu.Unlock()
	if req.TargetPath != want || req.TargetNodeUUID != target.UUID || req.SourceNodeUUID != holder.UUID ||
		req.SourcePath != "/models/explicit-main" || req.Key != store.ModelFileKey(row.ID) || req.TargetStorageID != row.StorageID {
		t.Fatalf("peer acquisition=%+v want target path=%q", req, want)
	}
	// A duplicate reconcile resumes the same target, not a fresh allocation.
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if count := hub.requestCount(); count != 1 {
		t.Fatalf("duplicate reconcile sent %d peer acquisitions", count)
	}
	waitForHubSync(t, hub)
	if err := st.UpdateModelFileStatus(target.ID, proto.ModelFileStatus{
		Key: req.Key, State: proto.MFPresent, Path: filepath.Join("/var/lib/gpuworks/models", want),
		TransferID: req.TransferID, BytesTotal: 12, BytesDone: 12,
	}); err != nil {
		t.Fatal(err)
	}
	c.onEvent(bus.Event{Topic: "modelfile", Key: req.Key})
	waitForModelSyncIdle(t, c, req.Key)
}
