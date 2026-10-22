package store

import "testing"

func TestPinModelRevisionOnlyForAnUnpinnedModelWithOneCopy(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	commit := "25d1d1fa49de445ab1f3a41f3537d7dad222826e"
	node, err := st.RegisterNode("pin-node", HashToken("t"), "pin-node")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.RegisterNode("pin-other", HashToken("o"), "pin-other")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterModelFile(FileRef{ModelID: model.ID, NodeID: node.ID}, "/models/m", 10); err != nil {
		t.Fatal(err)
	}
	// A second copy elsewhere: no pin.
	if _, err := st.RegisterModelFile(FileRef{ModelID: model.ID, NodeID: other.ID}, "/models/m", 10); err != nil {
		t.Fatal(err)
	}
	if pinned, err := st.PinModelRevision(model.ID, commit); err != nil || pinned {
		t.Fatalf("with two copies: pinned=%v err=%v", pinned, err)
	}
	if err := st.db.Where("model_id = ? AND node_id = ?", model.ID, other.ID).Delete(&ModelFile{}).Error; err != nil {
		t.Fatal(err)
	}
	if pinned, err := st.PinModelRevision(model.ID, commit); err != nil || !pinned {
		t.Fatalf("one copy: pinned=%v err=%v", pinned, err)
	}
	if got, _ := st.GetModel(model.ID); got.Revision != commit {
		t.Fatalf("revision = %q", got.Revision)
	}
	// Already pinned: no change.
	if pinned, _ := st.PinModelRevision(model.ID, "0123456789abcdef0123456789abcdef01234567"); pinned {
		t.Fatal("a pinned model was re-pinned")
	}
	// Another row already has the identity: no pin.
	clashing, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "org/m", Revision: "", Name: "m2"})
	if err != nil {
		t.Fatal(err)
	}
	if pinned, err := st.PinModelRevision(clashing.ID, commit); err != nil || pinned {
		t.Fatalf("clash: pinned=%v err=%v", pinned, err)
	}
}
