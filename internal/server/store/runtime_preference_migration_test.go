package store

import "testing"

func TestRuntimePreferenceMigrationAddsColumnAndIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	if !st.DB().Migrator().HasColumn(&Node{}, "RuntimePreference") {
		t.Fatal("runtime preference column missing")
	}
	if err := st.SetNodeRuntimePreference("missing", "docker"); err != ErrNotFound {
		t.Fatalf("missing node error = %v", err)
	}
	n, _, err := st.CreateNodePending("runtime-preference")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeRuntimePreference(n.UUID, "docker"); err != nil {
		t.Fatal(err)
	}
	got, err := st.NodeByUUID(n.UUID)
	if err != nil || got.RuntimePreference != "docker" {
		t.Fatalf("runtime preference = %q err=%v", got.RuntimePreference, err)
	}
	if err := st.SetNodeRuntimePreference(n.UUID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.NodeByUUID(n.UUID)
	if got.RuntimePreference != "auto" {
		t.Fatalf("empty preference = %q, want auto", got.RuntimePreference)
	}
}
