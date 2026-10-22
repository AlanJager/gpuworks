package store

import "testing"

func TestDirectImageTransfersForNodeKeepsOnlyRelatedPendingPeers(t *testing.T) {
	st := newTestStore(t)
	image, err := st.UpsertImage(&Image{ImageRef: "example:recovery", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rows := []ImageNode{
		{ImageVersionID: image.ID, NodeID: 1, SourceNodeID: 2, State: ImageStateSyncing, TransferID: "target", RateLimitKiB: 123},
		{ImageVersionID: image.ID, NodeID: 3, SourceNodeID: 1, State: ImageStateSyncing, TransferID: "source"},
		{ImageVersionID: image.ID, NodeID: 4, SourceNodeID: 2, State: ImageStateSyncing, TransferID: "unrelated"},
		{ImageVersionID: image.ID, NodeID: 5, SourceNodeID: 1, State: ImageStatePresent},
		{ImageVersionID: image.ID, NodeID: 6, SourceNodeID: 1, State: ImageStateFailed},
		{ImageVersionID: image.ID, NodeID: 7, SourceNodeID: 1, State: ImageStatePulling},
	}
	if err := st.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got, err := st.DirectImageTransfersForNode(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pending peers: %+v", got)
	}
	byToken := map[string]ImageNode{}
	for _, row := range got {
		byToken[row.TransferID] = row
		if row.ImageID != image.ID {
			t.Fatalf("image identity lost: %+v", row)
		}
	}
	if byToken["target"].RateLimitKiB != 123 || byToken["source"].SourceNodeID != 1 {
		t.Fatalf("transfer parameters lost: %+v", got)
	}
	if err := st.db.Model(&ImageNode{}).Where("node_id = ?", 1).Update("source_node_id", 0).Error; err != nil {
		t.Fatal(err)
	}
	got, err = st.DirectImageTransfersForNode(1)
	if err != nil || len(got) != 1 || got[0].TransferID != "source" {
		t.Fatalf("non-peer transfer was resumed: %+v %v", got, err)
	}
	if err := st.db.Migrator().DropTable(&ImageNode{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DirectImageTransfersForNode(1); err == nil {
		t.Fatal("query failure was hidden")
	}
}
