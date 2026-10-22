package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestNodesByUUIDReadsOnlyTheNamedNodes(t *testing.T) {
	st, err := Open("file:node_lookup?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, uuid := range []string{"n-a", "n-b", "n-c"} {
		if _, err := st.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := st.NodesByUUID([]string{"n-c", "n-a", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].UUID != "n-c" || nodes[1].UUID != "n-a" {
		t.Fatalf("nodes = %+v, want n-c then n-a (newest first)", nodes)
	}
	if none, err := st.NodesByUUID(nil); err != nil || none != nil {
		t.Fatalf("no names: %v %v", none, err)
	}
	if n, err := st.CountNodes(); err != nil || n != 3 {
		t.Fatalf("CountNodes = %d, %v", n, err)
	}
}

func TestModelFilesForModelOnNodes(t *testing.T) {
	st, err := Open("file:model_files_on_nodes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, row := range []ModelFile{{ModelID: 1, NodeID: 1, Path: "/a"}, {ModelID: 1, NodeID: 2, Path: "/b"}, {ModelID: 2, NodeID: 1, Path: "/c"}, {ModelID: 1, NodeID: 0, Path: "/shared"}} {
		row := row
		if err := st.DB().Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	files, err := st.ModelFilesForModelOnNodes(1, []uint{1})
	if err != nil || len(files) != 1 || files[0].Path != "/a" {
		t.Fatalf("files = %+v, %v", files, err)
	}
	if none, err := st.ModelFilesForModelOnNodes(1, nil); err != nil || none != nil {
		t.Fatalf("no nodes: %v %v", none, err)
	}
}
