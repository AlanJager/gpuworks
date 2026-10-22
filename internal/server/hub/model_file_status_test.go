package hub

import (
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelPreparationRequiresCurrentSession(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	model, err := h.store.UpsertModel(&store.Model{Source: "local", SourceID: "session-copy"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := h.store.PrepareModelCopy(store.FileRef{ModelID: model.ID, NodeID: node.ID}, "copy")
	if err != nil {
		t.Fatal(err)
	}
	old, current := &Session{hub: h, NodeUUID: uuid, nodeID: node.ID}, &Session{hub: h, NodeUUID: uuid, nodeID: node.ID}
	h.sessions[uuid] = current
	nodeReads := 0
	if err := h.store.DB().Callback().Query().Before("gorm:query").Register("preparation_identity_reads", func(db *gorm.DB) {
		if db.Statement.Table == "nodes" {
			nodeReads++
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer h.store.DB().Callback().Query().Remove("preparation_identity_reads")
	defer func() {
		if nodeReads != 0 {
			t.Errorf("progress reloaded authenticated node identity %d times", nodeReads)
		}
	}()
	report := proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: file.TransferID, State: proto.MFDownloading, BytesTotal: 100, BytesDone: 10, Preparation: &proto.ModelPreparation{Attempt: "a", Phase: proto.ModelPhaseCopying, TotalKnown: true}}
	if err := h.applyModelFileStatus(current, report); err != nil {
		t.Fatal(err)
	}
	report.State = proto.MFPresent
	report.Preparation = nil
	if err := h.applyModelFileStatus(old, report); err != nil {
		t.Fatal(err)
	}
	actual, _ := h.store.ModelFileByID(file.ID)
	if actual.State != proto.MFDownloading {
		t.Fatal("stale session installed model")
	}
	if observation, ok := h.ModelCopyObservation(actual); !ok || observation.BytesDone != 10 {
		t.Fatal("stale session replaced observation")
	}
	h.mu.Lock()
	h.copies.ForgetNode(node.ID)
	h.mu.Unlock()
	if err := h.applyModelFileStatus(old, report); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.ModelCopyObservation(actual); ok {
		t.Fatal("old session recreated disconnected observation")
	}
}
