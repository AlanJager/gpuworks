package controller

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestEnsureDeploymentModelsPresentStopsRepushingAfterModelFileFailed proves
// that a model file which already failed a direct download (proto.MFFailed,
// proto.ModelAcquireDownload — trySyncModelFromPeer bails out immediately for
// this acquisition mode) is never re-pushed to the agent again: without this,
// reconcileDeployment re-issues the identical doomed download forever with no
// backoff and no operator-visible signal.
func TestEnsureDeploymentModelsPresentStopsRepushingAfterModelFileFailed(t *testing.T) {
	st, err := store.Open("file:controller_model_file_failed_no_repush?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("model-failed-node", store.HashToken("token"), "model-failed-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/blocked", Revision: "main", Name: "blocked", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "huggingface/fixture_blocked-main",
		State: proto.MFFailed, Acquisition: proto.ModelAcquireDownload, Message: "404: repository not found",
	}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "blocked", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)

	for attempt := 0; attempt < 3; attempt++ {
		_, ready, err := c.ensureDeploymentModelsPresent(context.Background(), node, dep, model)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if ready {
			t.Fatalf("attempt %d: model reported ready, want blocked", attempt)
		}
	}
	if n := len(hub.updates[node.UUID]); n != 0 {
		t.Fatalf("pushed %d model file update(s) to a definitively failed download, want 0: %+v", n, hub.updates[node.UUID])
	}
}

// TestEnsureDeploymentModelsPresentPushesPendingModelFile is the control case:
// a model file that has never been attempted (proto.MFPending) must still be
// pushed, proving the failed-state check above does not disable pushes
// generally.
func TestEnsureDeploymentModelsPresentPushesPendingModelFile(t *testing.T) {
	st, err := store.Open("file:controller_model_file_pending_pushes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("model-pending-node", store.HashToken("token"), "model-pending-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/pending", Revision: "main", Name: "pending", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "huggingface/fixture_pending-main",
		State: proto.MFPending, Acquisition: proto.ModelAcquireDownload,
	}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "pending", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)

	_, ready, err := c.ensureDeploymentModelsPresent(context.Background(), node, dep, model)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("model reported ready, want not-yet-present")
	}
	if n := len(hub.updates[node.UUID]); n != 1 {
		t.Fatalf("pushed %d model file update(s) for a pending download, want 1: %+v", n, hub.updates[node.UUID])
	}
}
