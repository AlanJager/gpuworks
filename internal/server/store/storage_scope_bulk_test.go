package store

import (
	"reflect"
	"testing"
)

// EffectiveStorages answers for many nodes what StoragesForNode answers for
// one: the storages that apply to each, with that node's own overrides. A
// node-specific cache cap, a label target and a node the storage does not
// apply to are each told apart, from one read of the storages and targets.
func TestEffectiveStoragesMatchesTheSingleNodeAnswer(t *testing.T) {
	f := newStorageFixture(t)
	all := false
	small := int64(8 << 30)
	targets := []StorageNode{{LabelKey: "pool", LabelValue: "a"}, {NodeID: f.n1.ID, OverrideOnly: true, CacheMaxBytes: &small}, {NodeID: f.n2.ID}}
	if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &all, Targets: &targets}); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.n1, err = f.st.SetNodeLabels(f.n1.UUID, map[string]string{"pool": "a"}); err != nil {
		t.Fatal(err)
	}
	nodes := []*Node{f.n1, f.n2}
	bulk, err := f.st.EffectiveStorages(nodes)
	if err != nil {
		t.Fatal(err)
	}
	caps := map[uint]int64{}
	for _, node := range nodes {
		single, err := f.st.StoragesForNode(node)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(bulk[node.ID], single) {
			t.Fatalf("node %s: bulk %+v, single %+v", node.UUID, bulk[node.ID], single)
		}
		for _, storage := range single {
			if storage.Storage.ID == f.cache.ID {
				caps[node.ID] = storage.CacheMaxBytes
			}
		}
	}
	if caps[f.n1.ID] != small || caps[f.n2.ID] == small {
		t.Fatalf("cache caps = %v, want only the first node's overridden to %d", caps, small)
	}
	if none, err := f.st.EffectiveStorages(nil); err != nil || len(none) != 0 {
		t.Fatalf("no nodes: %+v %v", none, err)
	}
}
