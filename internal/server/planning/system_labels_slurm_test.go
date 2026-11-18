package planning

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func slurmStoreWithTwoEntries(t *testing.T) (st *store.Store, prod, lab *store.SlurmCluster, nodes []store.Node) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mk := func(name string) *store.SlurmCluster {
		c, err := st.CreateSlurmCluster(store.SlurmCluster{Name: name, ServerURL: "https://gw.example:8800", RunAs: "root",
			InstallDir: "/var/lib/gpuworks", ScriptDir: "/opt/gpuworks-slurm", IdleReturnSeconds: 60, PendingTimeoutSeconds: 600})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	prod, lab = mk("hpc-prod"), mk("hpc-lab")
	for _, e := range []struct {
		cluster *store.SlurmCluster
		node    string
	}{{prod, "gpu001"}, {prod, "gpu002"}, {lab, "gpu001"}} {
		n, _, err := st.EnrollSlurmNode(e.cluster.Token, "demo-"+e.cluster.Name, e.node)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, *n)
	}
	manual, err := st.RegisterNode("manual-uuid", store.HashToken("t"), "manual-host")
	if err != nil {
		t.Fatal(err)
	}
	// A node installed by hand on a machine that merely borrows cards from
	// Slurm knows its Slurm name but joined from no entry.
	manual.SlurmNode = "borrowed-1"
	nodes = append(nodes, *manual)
	return st, prod, lab, nodes
}

// A node that joined from an entry names it by UUID (a rename keeps the
// value) with the entry's name to show, and carries its name in Slurm; a node
// added any other way carries neither, even one that knows a Slurm node name.
func TestSystemLabelsNameTheSlurmPlacement(t *testing.T) {
	_, prod, _, nodes := slurmStoreWithTwoEntries(t)
	facts := NodeFacts{SlurmClusters: SlurmClusters{prod.ID: {UUID: prod.UUID, Name: "hpc-prod"}}}
	labels := SystemLabels(&nodes[0], facts)
	if display, code := displayOf(labels, nodelabel.KeySlurmCluster, prod.UUID); display != "hpc-prod" || code != "" {
		t.Fatalf("cluster display = %q/%q in\n%s", display, code, labelled(labels))
	}
	if display, _ := displayOf(labels, nodelabel.KeySlurmNode, "gpu001"); display != "gpu001" {
		t.Fatalf("node display = %q in\n%s", display, labelled(labels))
	}
	renamed := NodeFacts{SlurmClusters: SlurmClusters{prod.ID: {UUID: prod.UUID, Name: "renamed"}}}
	if display, _ := displayOf(SystemLabels(&nodes[0], renamed), nodelabel.KeySlurmCluster, prod.UUID); display != "renamed" {
		t.Fatalf("display after a rename = %q, want the new name under the same value", display)
	}
	if got := labelled(SystemLabels(&nodes[3], facts)); strings.Contains(got, "slurm") {
		t.Fatalf("a node that did not join from a Slurm entry carries\n%s", got)
	}
	// Entries that were not loaded leave the cluster label off, never wrong;
	// the node name needs nothing loaded.
	if got := labelled(SystemLabels(&nodes[0], NodeFacts{})); strings.Contains(got, nodelabel.KeySlurmCluster) || !strings.Contains(got, nodelabel.KeySlurmNode+"=gpu001") {
		t.Fatalf("labels without entries loaded =\n%s", got)
	}
}

// LoadNodeFacts reads the entries once, for the distinct ids of the nodes, and
// only when a requirement names the cluster label.
func TestLoadNodeFactsLoadsSlurmClustersOnlyWhenRequired(t *testing.T) {
	st, prod, lab, nodes := slurmStoreWithTwoEntries(t)
	if facts := LoadNodeFacts(st, nil, nodes, nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}}); facts.SlurmClusters != nil {
		t.Fatalf("entries loaded for a requirement that does not read them: %v", facts.SlurmClusters)
	}
	reqs := nodelabel.Requirements{nodelabel.KeySlurmCluster: {prod.UUID}}
	facts := LoadNodeFacts(st, nil, nodes, reqs)
	if len(facts.SlurmClusters) != 2 || facts.SlurmClusters[prod.ID] != (SlurmClusterRef{UUID: prod.UUID, Name: "hpc-prod"}) || facts.SlurmClusters[lab.ID].Name != "hpc-lab" {
		t.Fatalf("entries = %v", facts.SlurmClusters)
	}
	var kept []string
	for i := range nodes {
		if MatchRequirements(&nodes[i], facts, reqs) == nil {
			kept = append(kept, nodes[i].SlurmNode)
		}
	}
	if strings.Join(kept, ",") != "gpu001,gpu002" || nodes[2].SlurmClusterID == prod.ID {
		t.Fatalf("slurm-cluster=%s kept %v, want only hpc-prod's two nodes", prod.UUID, kept)
	}
	if err := MatchRequirements(&nodes[0], NodeFacts{}, reqs); err == nil {
		t.Fatal("a requirement on the entry matched without the entries loaded")
	}
	// The node name alone selects across entries, with nothing loaded, and not
	// the node that only borrows cards.
	nodeReqs := nodelabel.Requirements{nodelabel.KeySlurmNode: {"gpu001"}}
	nodeFacts := LoadNodeFacts(st, nil, nodes, nodeReqs)
	if nodeFacts.SlurmClusters != nil {
		t.Fatal("the node name label loaded entries it does not read")
	}
	kept = nil
	for i := range nodes {
		if MatchRequirements(&nodes[i], nodeFacts, nodeReqs) == nil {
			kept = append(kept, nodes[i].UUID)
		}
	}
	if len(kept) != 2 || kept[0] != nodes[0].UUID || kept[1] != nodes[2].UUID {
		t.Fatalf("slurm-node=gpu001 kept %v", kept)
	}
	labels := RequirementLabels(reqs, []*store.Node{&nodes[0], &nodes[1], &nodes[2], &nodes[3]}, facts)
	if len(labels) != 1 || labels[0].Display != "hpc-prod" || labels[0].Nodes != 2 {
		t.Fatalf("requirement labels = %+v", labels)
	}
}

func TestLoadSlurmClustersSkipsNodesWithoutOne(t *testing.T) {
	st, _, _, nodes := slurmStoreWithTwoEntries(t)
	if got := LoadSlurmClusters(st, nodes[3:]); got != nil {
		t.Fatalf("entries of a node joined from none = %v", got)
	}
	if got := LoadSlurmClusters(st, nil); got != nil {
		t.Fatalf("entries of no node = %v", got)
	}
}
