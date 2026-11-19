package slurmcluster

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeViewStore answers the grouped reads views make and counts them.
type fakeViewStore struct {
	counts    map[uint]store.SlurmNodeCounts
	listed    map[uint]int
	unlisted  map[uint]int
	missing   []store.SlurmMissingNode
	blockers  map[uint]store.SlurmDeleteBlockers
	shared    map[uint]bool
	lastSeen  map[uint]time.Time
	calls     map[string]int
	seenAsked []uint
}

func (f *fakeViewStore) call(name string) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
}

func (f *fakeViewStore) SlurmClusterNodeCounts() (map[uint]store.SlurmNodeCounts, error) {
	f.call("counts")
	return f.counts, nil
}
func (f *fakeViewStore) SlurmGPUNodeCounts() (map[uint]int, error) {
	f.call("listed")
	return f.listed, nil
}
func (f *fakeViewStore) SlurmUnlistedNodeCounts(time.Time) (map[uint]int, error) {
	f.call("unlisted")
	return f.unlisted, nil
}
func (f *fakeViewStore) SlurmMissingNodes(...uint) ([]store.SlurmMissingNode, error) {
	f.call("missing")
	return f.missing, nil
}
func (f *fakeViewStore) SlurmClusterDeleteBlockers(...uint) (map[uint]store.SlurmDeleteBlockers, error) {
	f.call("blockers")
	return f.blockers, nil
}
func (f *fakeViewStore) SlurmClustersWithSharedStorage() (map[uint]bool, error) {
	f.call("shared")
	return f.shared, nil
}
func (f *fakeViewStore) SlurmClustersLastSeen(ids ...uint) (map[uint]time.Time, error) {
	f.call("lastSeen")
	f.seenAsked = append(f.seenAsked, ids...)
	return f.lastSeen, nil
}

// Views read the store in a fixed number of grouped queries however many
// entries there are, and ask when an agent was last seen only of entries
// whose agents are all offline with a list to date.
func TestViewsReadTheStoreOnceForAllEntries(t *testing.T) {
	m, now := atClock(t)
	seen := now.Add(-time.Hour)
	st := &fakeViewStore{
		counts:   map[uint]store.SlurmNodeCounts{},
		listed:   map[uint]int{},
		lastSeen: map[uint]time.Time{3: seen},
	}
	var rows []store.SlurmCluster
	for i := uint(1); i <= 200; i++ {
		rows = append(rows, store.SlurmCluster{ID: i, Name: fmt.Sprint("e", i), SlurmClusterName: "c", CreatedAt: now.Add(-time.Hour)})
		st.counts[i] = store.SlurmNodeCounts{Enrolled: 2, Online: 1}
		st.listed[i] = 2
	}
	st.counts[3] = store.SlurmNodeCounts{Enrolled: 2, Online: 0}
	views, err := m.Views(st, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, n := range st.calls {
		if n != 1 {
			t.Errorf("%s read %d times for 200 entries", name, n)
		}
	}
	if len(st.calls) != 7 || !reflect.DeepEqual(st.seenAsked, []uint{3}) {
		t.Fatalf("reads = %v, last seen asked of %v", st.calls, st.seenAsked)
	}
	if v := views[3]; v.ListAsOf == nil || !v.ListAsOf.Equal(seen) || v.State != StateActive {
		t.Fatalf("an entry with its agents offline = %+v", v)
	}
	if v := views[1]; v.ListAsOf != nil || v.Nodes != 2 || v.Online != 1 || v.GPUNodes != 2 {
		t.Fatalf("an entry with an agent online = %+v", v)
	}
}

// What a view says of an entry: GPU nodes counted with those enrolled
// before a list could name them, missing nodes (bounded, with why), delete
// blockers, and waiting and state for one with no node.
func TestViewOfAnEntry(t *testing.T) {
	m, now := atClock(t)
	st := &fakeViewStore{
		counts:   map[uint]store.SlurmNodeCounts{1: {Enrolled: 1, Online: 1}, 3: {Enrolled: 1, Online: 1}},
		listed:   map[uint]int{1: 3},
		unlisted: map[uint]int{1: 1},
		blockers: map[uint]store.SlurmDeleteBlockers{1: {Online: 1}, 2: {Instances: 2}},
		shared:   map[uint]bool{1: true},
		missing: []store.SlurmMissingNode{
			{ClusterID: 1, Name: "gpu1", State: "idle"},
			{ClusterID: 1, Name: "gpu2", Enrolled: true, NodeUUID: "u2", State: "down", Reason: "not responding"},
		},
	}
	for i := 0; i < MissingNodesLimit+5; i++ {
		st.missing = append(st.missing, store.SlurmMissingNode{ClusterID: 3, Name: fmt.Sprint("n", i)})
	}
	rows := []store.SlurmCluster{
		{ID: 1, SlurmClusterName: "c", CreatedAt: *now},
		{ID: 2, CreatedAt: now.Add(-time.Hour)},
		{ID: 3, SlurmClusterName: "c", CreatedAt: *now},
	}
	var asked []uint
	cloud := func(c store.SlurmCluster) *int { asked = append(asked, c.ID); return intp(0) }
	views, err := m.Views(st, rows, cloud)
	if err != nil {
		t.Fatal(err)
	}
	v := views[1]
	want := []MissingNode{{Name: "gpu1", Reason: MissingNotEnrolled, SlurmState: "idle"}, {Name: "gpu2", Reason: MissingOffline, NodeUUID: "u2", SlurmState: "down", SlurmReason: "not responding"}}
	if v.GPUNodes != 4 || v.Missing != 2 || !reflect.DeepEqual(v.MissingNodes, want) || !v.HasSharedStorage || v.DeleteAllowed || v.DeleteBlocker != BlockerOnline || v.Waiting.Reason != "" {
		t.Fatalf("entry with nodes = %+v", v)
	}
	if v := views[2]; v.DeleteBlocker != BlockerInstances || v.Waiting.Reason != WaitingNoComputeNodes || v.State != StateStalled {
		t.Fatalf("entry without nodes = %+v", v)
	}
	if v := views[3]; v.Missing != MissingNodesLimit+5 || len(v.MissingNodes) != MissingNodesLimit || !v.DeleteAllowed {
		t.Fatalf("entry with many missing = %d listed of %d, delete allowed %v", len(v.MissingNodes), v.Missing, v.DeleteAllowed)
	}
	if !reflect.DeepEqual(asked, []uint{2}) {
		t.Fatalf("the cloud was asked about %v; only an entry with no node needs it", asked)
	}
}
