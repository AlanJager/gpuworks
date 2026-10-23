package store

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestUnmanagedStoragePaths(t *testing.T) {
	st, err := Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	nodeA, err := st.RegisterNode("node-a", "token-a", "a")
	if err != nil {
		t.Fatal(err)
	}
	nodeB, err := st.RegisterNode("node-b", "token-b", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStorage(&Storage{
		Name: "mixed", Type: proto.StorageLocal, Enabled: true, AllNodes: false,
		Path: "/var/lib/gpuworks/models", CachePath: "/opt/outside/cache",
	}, []StorageNode{
		{NodeID: nodeA.ID, Path: "/opt/outside/node-a", CachePath: "/var/lib/gpuworks/cache-a"},
		{NodeID: nodeB.ID, Path: "/var/lib/gpuworks/node-b", CachePath: "/opt/outside/cache-b"},
	}); err != nil {
		t.Fatal(err)
	}
	// A sibling of the root is outside; the root directory itself is inside.
	if _, err := st.CreateStorage(&Storage{
		Name: "sibling", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/var/lib/gpuworks-other",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStorage(&Storage{
		Name: "rootish", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/var/lib/gpuworks",
	}, nil); err != nil {
		t.Fatal(err)
	}
	roots := []string{"/var/lib/gpuworks"}
	unmanaged, err := st.UnmanagedStoragePaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range unmanaged {
		got[row.Name+" "+row.Path] = true
	}
	want := []string{
		"default " + DefaultLocalStoragePath,
		"mixed /opt/outside/cache",
		"mixed /opt/outside/node-a",
		"mixed /opt/outside/cache-b",
		"sibling /var/lib/gpuworks-other",
	}
	for _, key := range want {
		if !got[key] {
			t.Errorf("missing %s", key)
		}
	}
	if len(unmanaged) != len(want) || len(got) != len(want) {
		t.Fatalf("unmanaged = %+v", unmanaged)
	}
	none, err := st.UnmanagedStoragePaths(nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty roots = %+v err=%v", none, err)
	}
}
