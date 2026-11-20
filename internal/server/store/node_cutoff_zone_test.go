package store

import (
	"slices"
	"testing"
	"time"
)

// The store writes times in UTC and SQLite compares them as text, so a cutoff
// a caller builds from time.Now() in the server's local zone must still mean
// the same instant: the node sweeps and the Slurm GPU node count answer the
// same for one instant in any zone, whatever zone the server runs in.
func TestNodeCutoffsMeanTheSameInstantInAnyZone(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	long, recent := now.Add(-time.Hour), now.Add(-time.Minute)
	cutoff := now.Add(-15 * time.Minute)

	c := newSlurmCluster(t, s, "hpc")
	for _, name := range []string{"slurm-old", "slurm-new"} {
		if _, _, err := s.EnrollSlurmNode(c.Token, "demo", name); err != nil {
			t.Fatal(err)
		}
	}
	// As the store writes them: UTC, enrolled and last seen an hour ago or a
	// minute ago. Both agents are gone; the list names neither.
	setNode := func(where string, args []any, created, seen time.Time) {
		t.Helper()
		if err := s.DB().Model(&Node{}).Where(where, args...).Updates(map[string]any{"state": NodeStateOffline, "created_at": created, "last_seen_at": seen}).Error; err != nil {
			t.Fatal(err)
		}
	}
	setNode("slurm_node = ?", []any{"slurm-old"}, long, long)
	setNode("slurm_node = ?", []any{"slurm-new"}, now, recent)
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "elsewhere", State: "idle", GPUs: 1}}); err != nil {
		t.Fatal(err)
	}

	k, err := s.CreateKubeCluster(KubeCluster{Name: "k", Namespace: "k", ServerURL: "https://gw", EnginePorts: "40000-42767", IdleReturnSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kube-old", "kube-new"} {
		if _, _, err := s.EnrollKubeNode(k.Token, "uid", name); err != nil {
			t.Fatal(err)
		}
	}
	setNode("kube_cluster_id = ? AND kube_node_name = ?", []any{k.ID, "kube-old"}, long, long)
	setNode("kube_cluster_id = ? AND kube_node_name = ?", []any{k.ID, "kube-new"}, now, recent)

	names := func(nodes []Node, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, n := range nodes {
			out = append(out, n.SlurmNode+n.KubeNodeName)
		}
		slices.Sort(out)
		return out
	}
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("east", 14*3600), time.FixedZone("west", -12*3600)} {
		at := cutoff.In(zone)
		unlisted, err := s.SlurmUnlistedNodeCounts(at)
		if err != nil {
			t.Fatal(err)
		}
		if unlisted[c.ID] != 1 {
			t.Errorf("%s: unlisted nodes enrolled since the cutoff = %v, want slurm-new only", zone, unlisted)
		}
		if got := names(s.OfflineSlurmNodes(at)); !slices.Equal(got, []string{"slurm-old"}) {
			t.Errorf("%s: Slurm nodes gone since before the cutoff = %v", zone, got)
		}
		if got := names(s.OfflineKubeNodes(at)); !slices.Equal(got, []string{"kube-old"}) {
			t.Errorf("%s: Kubernetes nodes gone since before the cutoff = %v", zone, got)
		}
	}
}
