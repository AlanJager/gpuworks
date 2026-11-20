package store

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestSlurmClusterDeleteBlockersCountOnlineAgentsAndNodesWithInstances(t *testing.T) {
	s := newTestStore(t)
	a := newSlurmCluster(t, s, "a")
	b := newSlurmCluster(t, s, "b")
	free := newSlurmCluster(t, s, "free")
	nodeOf := map[string]uint{}
	for _, c := range []*SlurmCluster{a, a, a, b, free} {
		name := "n" + string(rune('0'+len(nodeOf)))
		n, _, err := s.EnrollSlurmNode(c.Token, "demo-"+c.Name, name)
		if err != nil {
			t.Fatal(err)
		}
		nodeOf[name] = n.ID
	}
	setState := func(name, state string) {
		if err := s.DB().Model(&Node{}).Where("id = ?", nodeOf[name]).Update("state", state).Error; err != nil {
			t.Fatal(err)
		}
	}
	// a: n0 and n1 online; n2 offline with two live instances and one leaving. b: n3 offline with a leaving instance only. free: n4 offline, idle.
	setState("n0", NodeStateOnline)
	setState("n1", NodeStateOnline)
	setState("n2", NodeStateOffline)
	setState("n3", NodeStateOffline)
	setState("n4", NodeStateOffline)
	inst := func(uuid, node, state string) {
		if err := s.DB().Create(&Instance{UUID: uuid, NodeID: nodeOf[node], State: state}).Error; err != nil {
			t.Fatal(err)
		}
	}
	inst("i1", "n2", proto.InstRunning)
	inst("i2", "n2", proto.InstStarting)
	inst("i3", "n2", proto.InstRemoving)
	inst("i4", "n3", proto.InstPendingRemoval)

	got, err := s.SlurmClusterDeleteBlockers(a.ID, b.ID, free.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (SlurmDeleteBlockers{Online: 2, Instances: 1}); got[a.ID] != want {
		t.Fatalf("a = %+v, want %+v", got[a.ID], want)
	}
	// Instances count nodes holding them, not rows; a leaving row holds nothing.
	if _, held := got[b.ID]; held {
		t.Fatalf("b = %+v, want no blocker", got[b.ID])
	}
	if _, held := got[free.ID]; held {
		t.Fatalf("free = %+v, want no blocker", got[free.ID])
	}
	// Asking for one entry reads that entry only.
	if one, err := s.SlurmClusterDeleteBlockers(b.ID); err != nil || len(one) != 0 {
		t.Fatalf("b alone = %v, %v", one, err)
	}
	nodes, err := s.SlurmClusterNodes(a.ID)
	if err != nil || len(nodes) != 3 || nodes[0].ID != nodeOf["n0"] {
		t.Fatalf("a's nodes = %v, %v", nodes, err)
	}
}

// The blockers are read on every list of the entries, with the entries' ids:
// that never scans the node table.
func TestSlurmClusterDeleteBlockerQueriesUseTheNodeIndex(t *testing.T) {
	s := newTestStore(t)
	for name, query := range map[string]string{
		"online agents of the entries": "SELECT slurm_cluster_id, count(*) AS n FROM nodes WHERE slurm_cluster_id > 0 AND state = 'online' AND slurm_cluster_id IN (1, 2) GROUP BY slurm_cluster_id",
		"nodes with instances":         "SELECT nodes.slurm_cluster_id, count(DISTINCT nodes.id) FROM nodes JOIN instances ON instances.node_id = nodes.id AND instances.state NOT IN ('removing') WHERE nodes.slurm_cluster_id > 0 AND nodes.slurm_cluster_id IN (1, 2) GROUP BY nodes.slurm_cluster_id",
		"an entry's nodes":             "SELECT * FROM nodes WHERE slurm_cluster_id > 0 AND slurm_cluster_id = 1 ORDER BY id",
	} {
		var plan []struct{ Detail string }
		if err := s.DB().Raw("EXPLAIN QUERY PLAN " + query).Scan(&plan).Error; err != nil {
			t.Fatal(err)
		}
		used := false
		for _, p := range plan {
			if strings.HasPrefix(p.Detail, "SCAN nodes") {
				t.Errorf("%s scans the node table: %v", name, plan)
			}
			used = used || strings.Contains(p.Detail, "idx_node_slurm")
		}
		if !used {
			t.Errorf("%s does not use idx_node_slurm: %v", name, plan)
		}
	}
}

// A node that enrolls, or an instance that is synced, is tied to the other
// when the entry was made from the instance's cloud cluster; nothing else is.
func TestSlurmNodeAndCloudInstanceAreLinkedBothWays(t *testing.T) {
	s := newTestStore(t)
	acct := &Provider{Name: "ali", Kind: "aliyun"}
	if err := s.DB().Create(acct).Error; err != nil {
		t.Fatal(err)
	}
	entry, err := s.CreateSlurmCluster(SlurmCluster{Name: "ehpc", ServerURL: "https://gw", RunAs: "root", InstallDir: "/i", ScriptDir: "/s",
		IdleReturnSeconds: 60, PendingTimeoutSeconds: 600, ProviderID: &acct.ID, ProviderClusterID: "ehpc-1"})
	if err != nil {
		t.Fatal(err)
	}
	plain := newSlurmCluster(t, s, "plain")
	row := func(ext, name, cluster string) ProviderInstance {
		return ProviderInstance{ProviderID: acct.ID, ExternalID: ext, Name: name, HPCClusterID: cluster, HPCScheduler: "slurm"}
	}
	if err := s.SyncProviderInstances(acct.ID, []ProviderInstance{
		row("i-0", HPCInstanceName("ehpc-1", "gpu0"), "ehpc-1"), row("i-1", HPCInstanceName("ehpc-1", "gpu1"), "ehpc-1"),
		row("i-other", HPCInstanceName("ehpc-2", "gpu0"), "ehpc-2"), {ProviderID: acct.ID, ExternalID: "i-plain", Name: "gpu0"},
	}); err != nil {
		t.Fatal(err)
	}
	link := func(ext string) uint {
		r, err := s.ProviderInstanceByExternalID(acct.ID, ext)
		if err != nil || r == nil {
			t.Fatalf("%s: %v", ext, err)
		}
		return r.NodeID
	}
	n0, _, err := s.EnrollSlurmNode(entry.Token, "demo", "gpu0")
	if err != nil {
		t.Fatal(err)
	}
	// Enrolling again changes nothing; another entry's node of the same name is no one's instance.
	if _, _, err := s.EnrollSlurmNode(entry.Token, "demo", "gpu0"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollSlurmNode(plain.Token, "other", "gpu0"); err != nil {
		t.Fatal(err)
	}
	if link("i-0") != n0.ID || link("i-1") != 0 || link("i-other") != 0 || link("i-plain") != 0 {
		t.Fatalf("after enrolling gpu0: i-0 %d (node %d), i-1 %d, i-other %d, i-plain %d", link("i-0"), n0.ID, link("i-1"), link("i-other"), link("i-plain"))
	}
	// A later sync links the instance of a node that enrolled before it was listed.
	n1, _, err := s.EnrollSlurmNode(entry.Token, "demo", "gpu-late")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncProviderInstances(acct.ID, []ProviderInstance{
		row("i-0", HPCInstanceName("ehpc-1", "gpu0"), "ehpc-1"), row("i-1", HPCInstanceName("ehpc-1", "gpu1"), "ehpc-1"),
		row("i-late", HPCInstanceName("ehpc-1", "gpu-late"), "ehpc-1"),
	}); err != nil {
		t.Fatal(err)
	}
	if link("i-0") != n0.ID || link("i-late") != n1.ID || link("i-1") != 0 {
		t.Fatalf("after the sync: i-0 %d, i-late %d (node %d), i-1 %d", link("i-0"), link("i-late"), n1.ID, link("i-1"))
	}
}
