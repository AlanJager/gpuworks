package slurmsweep

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"
)

// fakeStore lists its nodes as offline until they are deleted, and fails where
// a test says so.
type fakeStore struct {
	nodes        []Node
	cloudNodes   []Node // offline nodes of cloud entries
	cloudErr     error
	listErr      error
	instances    map[uint]bool
	instancesErr map[uint]error
	deleteErr    map[string]error
	deleted      []string
}

func (f *fakeStore) OfflineNodes(time.Time) ([]Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []Node
	for _, n := range f.nodes {
		gone := false
		for _, d := range f.deleted {
			gone = gone || d == n.UUID
		}
		if !gone {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeStore) OfflineCloudNodes(time.Time) ([]Node, error) {
	if f.cloudErr != nil {
		return nil, f.cloudErr
	}
	var out []Node
	for _, n := range f.cloudNodes {
		if !slices.Contains(f.deleted, n.UUID) {
			out = append(out, n)
		}
	}
	return out, nil
}

// fakeCloud lists each entry's cloud nodes, or fails for an entry.
type fakeCloud struct {
	names map[uint][]string
	err   map[uint]error
	asked []uint
}

func (c *fakeCloud) NodeNames(_ context.Context, cluster uint) ([]string, error) {
	c.asked = append(c.asked, cluster)
	return c.names[cluster], c.err[cluster]
}

func (f *fakeStore) HasInstances(id uint) (bool, error) { return f.instances[id], f.instancesErr[id] }

func (f *fakeStore) DeleteNode(uuid string) error {
	if err := f.deleteErr[uuid]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, uuid)
	return nil
}

func sweeperOver(f *fakeStore, kicked *[]string) *Sweeper {
	return New(slog.New(slog.DiscardHandler), f, nil, func(u string) { *kicked = append(*kicked, u) })
}

func TestSweepRemovesANodeOnlyOnTheSecondSweepThatFindsItGone(t *testing.T) {
	f := &fakeStore{nodes: []Node{{ID: 1, UUID: "a", SlurmNode: "gpu1"}}}
	var kicked []string
	s := sweeperOver(f, &kicked)
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 0 {
		t.Fatalf("removed on the first sweep: %v", f.deleted)
	}
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 1 || f.deleted[0] != "a" || len(kicked) != 1 || kicked[0] != "a" {
		t.Fatalf("deleted %v, kicked %v", f.deleted, kicked)
	}
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 1 || len(kicked) != 1 {
		t.Fatalf("a removed node was removed again: %v %v", f.deleted, kicked)
	}
}

func TestSweepLeavesNodesItCannotSettle(t *testing.T) {
	f := &fakeStore{
		nodes:        []Node{{ID: 1, UUID: "busy"}, {ID: 2, UUID: "unreadable"}, {ID: 3, UUID: "stuck"}},
		instances:    map[uint]bool{1: true},
		instancesErr: map[uint]error{2: errors.New("database is locked")},
		deleteErr:    map[string]error{"stuck": errors.New("database is locked")},
	}
	var kicked []string
	s := sweeperOver(f, &kicked)
	for i := 0; i < 3; i++ {
		s.Sweep(context.Background(), time.Now())
	}
	if len(f.deleted) != 0 || len(kicked) != 0 {
		t.Fatalf("deleted %v, kicked %v: a node with instances, an unreadable one or one whose delete failed was dropped", f.deleted, kicked)
	}
	// The failed delete is tried again on the next sweep, and kicks only once it worked.
	f.deleteErr = nil
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 1 || f.deleted[0] != "stuck" || len(kicked) != 1 {
		t.Fatalf("deleted %v, kicked %v", f.deleted, kicked)
	}
}

func TestAFailedListRemovesNothingAndKeepsWhatTheSweepBeforeFound(t *testing.T) {
	f := &fakeStore{nodes: []Node{{ID: 1, UUID: "a"}}}
	var kicked []string
	s := sweeperOver(f, &kicked)
	s.Sweep(context.Background(), time.Now())
	f.listErr = errors.New("database is locked")
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 0 || len(kicked) != 0 {
		t.Fatalf("a failed list removed %v", f.deleted)
	}
	f.listErr = nil
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 1 || len(kicked) != 1 {
		t.Fatalf("deleted %v, kicked %v", f.deleted, kicked)
	}
}

// The node that left was the last one with an agent (E-HPC, 2026-11-18): no
// agent reports a list without it, so only the cloud can tell it is gone. A
// node the cloud still has stays, however long it is offline; an entry whose
// cloud list fails removes nothing; each entry is asked once a sweep.
func TestACloudEntrysNodeTheCloudNoLongerHasIsRemoved(t *testing.T) {
	f := &fakeStore{cloudNodes: []Node{
		{ID: 1, UUID: "gone", ClusterID: 7, SlurmNode: "compute001"},
		{ID: 2, UUID: "stopped", ClusterID: 7, SlurmNode: "compute002"},
		{ID: 3, UUID: "unknown", ClusterID: 8, SlurmNode: "compute001"},
	}}
	cloud := &fakeCloud{names: map[uint][]string{7: {"compute002", "compute003"}}, err: map[uint]error{8: errors.New("Throttling.User")}}
	var kicked []string
	s := New(slog.New(slog.DiscardHandler), f, cloud, func(u string) { kicked = append(kicked, u) })
	s.Sweep(context.Background(), time.Now())
	if len(f.deleted) != 0 {
		t.Fatalf("removed on the first sweep: %v", f.deleted)
	}
	if !slices.Equal(cloud.asked, []uint{7, 8}) {
		t.Fatalf("asked the cloud about %v; want each entry once", cloud.asked)
	}
	s.Sweep(context.Background(), time.Now())
	if !slices.Equal(f.deleted, []string{"gone"}) || !slices.Equal(kicked, []string{"gone"}) {
		t.Fatalf("deleted %v, kicked %v; want only the node the cloud no longer has", f.deleted, kicked)
	}
}

// A node found gone from its list is not asked about again in the cloud, and
// is removed once.
func TestANodeGoneFromBothIsRemovedOnce(t *testing.T) {
	n := Node{ID: 1, UUID: "a", ClusterID: 7, SlurmNode: "gpu1"}
	f := &fakeStore{nodes: []Node{n}, cloudNodes: []Node{n}}
	cloud := &fakeCloud{names: map[uint][]string{7: {}}}
	s := New(slog.New(slog.DiscardHandler), f, cloud, func(string) {})
	s.Sweep(context.Background(), time.Now())
	s.Sweep(context.Background(), time.Now())
	if !slices.Equal(f.deleted, []string{"a"}) || len(cloud.asked) != 0 {
		t.Fatalf("deleted %v, asked the cloud %v", f.deleted, cloud.asked)
	}
}

// When the cloud's nodes cannot be read, the list's verdicts still stand.
func TestAFailedCloudReadKeepsTheListsVerdicts(t *testing.T) {
	f := &fakeStore{nodes: []Node{{ID: 1, UUID: "a"}}, cloudErr: errors.New("database is locked")}
	s := New(slog.New(slog.DiscardHandler), f, &fakeCloud{}, func(string) {})
	s.Sweep(context.Background(), time.Now())
	s.Sweep(context.Background(), time.Now())
	if !slices.Equal(f.deleted, []string{"a"}) {
		t.Fatalf("deleted %v", f.deleted)
	}
}
