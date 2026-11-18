package store

import (
	"testing"
	"time"
)

func TestActiveImageTransfersReadsCommittedWALFactsWithoutWaitingForWriter(t *testing.T) {
	st := openFileBackedTestStore(t)
	image, err := st.UpsertImage(&Image{Name: "runner", ImageRef: "fixture/runner:v1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("image-node", HashToken("image-node"), "image-node")
	if err != nil {
		t.Fatal(err)
	}
	active, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	tx := st.DB().Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Model(&ImageNode{}).Where("id = ?", active.ID).Update("state", ImageStateFailed).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		rows, err := st.ActiveImageTransfers()
		if err == nil && (len(rows) != 1 || rows[0].TransferID != "active") {
			t.Errorf("uncommitted terminal state leaked: %+v", rows)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("image transfer read blocked behind writer")
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	rows, err := st.ActiveImageTransfers()
	if err != nil || len(rows) != 0 {
		t.Fatalf("committed terminal state invisible: %+v %v", rows, err)
	}
	// A lost reader is an error, never an empty inventory that pretends every
	// transfer has completed.
	pool, err := st.readDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActiveImageTransfers(); err == nil {
		t.Fatal("closed reader reported successful empty image facts")
	}
}
