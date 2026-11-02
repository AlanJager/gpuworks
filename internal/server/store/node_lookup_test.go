package store

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestCountNodesUnseenSinceComparesInstantsAcrossTimeZones(t *testing.T) {
	st, err := Open("file:node_seen_timezones?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cutoff := time.Date(2026, 11, 2, 4, 0, 0, 123456000, time.UTC)
	for _, uuid := range []string{"never", "before", "equal", "after"} {
		if _, err := st.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB().Model(&Node{}).Where("uuid = ?", "never").Update("last_seen_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.TouchNodesAt(map[string]time.Time{
		"before": cutoff.Add(-time.Second), "equal": cutoff, "after": cutoff.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("UTC+08", 8*60*60), time.FixedZone("UTC-07", -7*60*60)} {
		t.Run(zone.String(), func(t *testing.T) {
			// The instant and stored heartbeats stay the same: only the
			// caller's representation of the cutoff changes.
			count, err := st.CountNodesUnseenSince(cutoff.In(zone))
			if err != nil || count != 2 {
				t.Fatalf("unseen at %s = %d, %v; want only never and before", cutoff.In(zone), count, err)
			}
		})
	}
}

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
