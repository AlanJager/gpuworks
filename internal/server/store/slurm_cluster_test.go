package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func newSlurmCluster(t *testing.T, s *Store, name string) *SlurmCluster {
	t.Helper()
	c, err := s.CreateSlurmCluster(SlurmCluster{Name: name, ServerURL: "http://gw:8800", RunAs: SlurmDefaultRunAs,
		InstallDir: SlurmDefaultInstallDir, ScriptDir: SlurmDefaultScriptDir,
		IdleReturnSeconds: SlurmDefaultIdleReturnSeconds, PendingTimeoutSeconds: SlurmDefaultPendingTimeoutSeconds})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnrollSlurmNodeIsIdempotentAndBindsTheSlurmCluster(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	if c.Token == "" || c.TokenHash != HashToken(c.Token) || c.UUID == "" || c.SlurmClusterName != "" {
		t.Fatalf("created cluster = %+v", c)
	}
	n1, got, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SlurmClusterName != "demo" || n1.SlurmClusterID != c.ID || n1.SlurmNode != "gpu-1" || n1.Token == "" || n1.ConnectMode != "agent_dial" {
		t.Fatalf("first enrollment: node %+v cluster %+v", n1, got)
	}
	n1again, _, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-1")
	if err != nil || n1again.UUID != n1.UUID || n1again.Token != n1.Token {
		t.Fatalf("the same node enrolling again = %+v, %v (want the same node)", n1again, err)
	}
	n2, _, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-2")
	if err != nil || n2.UUID == n1.UUID {
		t.Fatalf("second node = %+v, %v", n2, err)
	}
	if _, _, err := s.EnrollSlurmNode(c.Token, "another", "gpu-3"); !errors.Is(err, ErrSlurmClusterMismatch) {
		t.Fatalf("a token used from another Slurm cluster = %v, want ErrSlurmClusterMismatch", err)
	}
	if _, _, err := s.EnrollSlurmNode("nope", "demo", "gpu-1"); !errors.Is(err, ErrSlurmTokenUnknown) {
		t.Fatalf("unknown token = %v", err)
	}
	for _, bad := range [][2]string{{"", "gpu"}, {"demo", ""}} {
		if _, _, err := s.EnrollSlurmNode(c.Token, bad[0], bad[1]); err == nil || errors.Is(err, ErrSlurmTokenUnknown) {
			t.Fatalf("enroll(%q, %q) = %v, want a validation error", bad[0], bad[1], err)
		}
	}
	counts, err := s.SlurmClusterNodeCounts()
	if err != nil || counts[c.ID] != (SlurmNodeCounts{Enrolled: 2}) {
		t.Fatalf("node counts = %v, %v", counts, err)
	}
	if err := s.RevokeSlurmCluster(c.UUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-4"); !errors.Is(err, ErrSlurmTokenUnknown) {
		t.Fatalf("revoked token enrolled a node: %v", err)
	}
	if _, err := s.SlurmClusterByToken(c.Token); !errors.Is(err, ErrSlurmTokenUnknown) {
		t.Fatalf("revoked token found its cluster: %v", err)
	}
}

// Two entries for one Slurm cluster do not both take a node: its cards would
// have two managers. The node moves once its agent has been gone for a while.
func TestEnrollSlurmNodeRefusesANodeAnotherEntryHolds(t *testing.T) {
	s := newTestStore(t)
	a, b := newSlurmCluster(t, s, "a"), newSlurmCluster(t, s, "b")
	n, _, err := s.EnrollSlurmNode(a.Token, "demo", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollSlurmNode(b.Token, "demo", "gpu-1"); !errors.Is(err, ErrSlurmNodeTaken) {
		t.Fatalf("a node held by an online-or-recent entry = %v, want ErrSlurmNodeTaken", err)
	} else {
		var taken *SlurmNodeTakenError
		if !errors.As(err, &taken) || taken.Entry != "a" {
			t.Fatalf("taken error = %#v", err)
		}
	}
	// Another Slurm cluster's node of the same name is no clash.
	other := newSlurmCluster(t, s, "c")
	if _, _, err := s.EnrollSlurmNode(other.Token, "elsewhere", "gpu-1"); err != nil {
		t.Fatalf("same node name in another Slurm cluster: %v", err)
	}
	old := time.Now().Add(-2 * SlurmNodeTakenFor)
	if err := s.DB().Model(&Node{}).Where("uuid = ?", n.UUID).Updates(map[string]any{"state": NodeStateOffline, "last_seen_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollSlurmNode(b.Token, "demo", "gpu-1"); err != nil {
		t.Fatalf("a node whose agent is long gone cannot move: %v", err)
	}
}

// A status report never rewrites an enrolled node's Slurm name: an agent that
// could not ask Slurm yet reports none, and enrollment finds the node by it.
func TestStatusReportKeepsAnEnrolledNodesSlurmName(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	n, _, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	for _, name := range []string{"", "another-name", "gpu-1"} {
		if err := s.UpdateNodeStatus(n.UUID, proto.NodeStatus{GPUProbeOK: &ok, Sys: proto.SysInfo{RuntimeDriver: "docker", SlurmNode: name}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.NodeByUUID(n.UUID)
		if err != nil || got.SlurmNode != "gpu-1" || got.SlurmClusterID != c.ID {
			t.Fatalf("after a report naming %q: %+v, %v", name, got, err)
		}
	}
	again, _, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-1")
	if err != nil || again.UUID != n.UUID {
		t.Fatalf("re-enrollment after the reports = %+v, %v", again, err)
	}
	// A hand-installed node (no entry) still reports its own Slurm name.
	if _, err := s.RegisterNode("uuid-hand", HashToken("t"), "hand"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("uuid-hand", proto.NodeStatus{GPUProbeOK: &ok, Sys: proto.SysInfo{RuntimeDriver: "docker", SlurmNode: "hand-01"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.NodeByUUID("uuid-hand"); got.SlurmNode != "hand-01" {
		t.Fatalf("hand-installed node's Slurm name = %q", got.SlurmNode)
	}
}

func TestSlurmClusterCreateUpdateAndDelete(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	if _, err := s.CreateSlurmCluster(SlurmCluster{Name: "hpc"}); !errors.Is(err, ErrSlurmClusterNameTaken) {
		t.Fatalf("duplicate name = %v", err)
	}
	other := newSlurmCluster(t, s, "other")
	got, err := s.UpdateSlurmCluster(SlurmCluster{UUID: c.UUID, Name: "renamed", AccountUser: "svc", SbatchArgs: "-A ai",
		IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, ServerURL: "http://ignored", RunAs: "ignored", InstallDir: "/ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.AccountUser != "svc" || got.SbatchArgs != "-A ai" || got.IdleReturnSeconds != 120 || got.PendingTimeoutSeconds != 900 {
		t.Fatalf("edited = %+v", got)
	}
	if got.ServerURL != "http://gw:8800" || got.RunAs != "root" || got.InstallDir != SlurmDefaultInstallDir || got.Token != c.Token {
		t.Fatalf("the fixed settings changed: %+v", got)
	}
	if _, err := s.UpdateSlurmCluster(SlurmCluster{UUID: c.UUID, Name: other.Name}); !errors.Is(err, ErrSlurmClusterNameTaken) {
		t.Fatalf("rename onto another entry = %v", err)
	}
	if _, err := s.UpdateSlurmCluster(SlurmCluster{UUID: "nope", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing entry = %v", err)
	}

	node, _, err := s.EnrollSlurmNode(c.Token, "demo", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "gpu-1", State: "idle", GPUs: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSlurmCluster(c.UUID); !errors.Is(err, ErrSlurmClusterHasNodes) {
		t.Fatalf("delete with nodes = %v", err)
	}
	if err := s.DeleteNode(node.UUID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSlurmCluster(c.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SlurmClusterByUUID(c.UUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted entry still found: %v", err)
	}
	if counts, _ := s.SlurmGPUNodeCounts(); counts[c.ID] != 0 {
		t.Fatalf("the deleted entry's node list stayed: %v", counts)
	}
	if err := s.DeleteSlurmCluster(c.UUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestSlurmClusterFromACloudClusterIsMadeOnce(t *testing.T) {
	s := newTestStore(t)
	id := uint(7)
	if _, err := s.CreateSlurmCluster(SlurmCluster{Name: "ehpc", ProviderID: &id, ProviderClusterID: "ehpc-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSlurmCluster(SlurmCluster{Name: "again", ProviderID: &id, ProviderClusterID: "ehpc-1"}); !errors.Is(err, ErrSlurmClusterSourceTaken) {
		t.Fatalf("second entry for one cloud cluster = %v", err)
	}
	if _, err := s.CreateSlurmCluster(SlurmCluster{Name: "other", ProviderID: &id, ProviderClusterID: "ehpc-2"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceSlurmGPUNodesWritesOnlyWhenTheListChanges(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	list := []SlurmGPUNode{{Name: "gpu-2", State: "idle", GPUs: 2}, {Name: "gpu-1", State: "drained", Reason: "gres", GPUs: 1}}
	if change, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || !change.Changed() {
		t.Fatalf("first list: change=%+v err=%v", change, err)
	}
	var firstIDs []uint
	if err := s.DB().Model(&SlurmGPUNode{}).Order("id").Pluck("id", &firstIDs).Error; err != nil || len(firstIDs) != 2 {
		t.Fatalf("rows = %v, %v", firstIDs, err)
	}
	// The same list in another order is the same list: nothing is written.
	reordered := []SlurmGPUNode{list[1], list[0]}
	if change, err := s.ReplaceSlurmGPUNodes(c.ID, reordered); err != nil || change.Changed() {
		t.Fatalf("same list: change=%+v err=%v", change, err)
	}
	var sameIDs []uint
	if err := s.DB().Model(&SlurmGPUNode{}).Order("id").Pluck("id", &sameIDs).Error; err != nil || len(sameIDs) != 2 || sameIDs[0] != firstIDs[0] || sameIDs[1] != firstIDs[1] {
		t.Fatalf("rows were rewritten by an unchanged list: %v -> %v", firstIDs, sameIDs)
	}
	// A changed state is a change; so is a shorter list; so is an empty one.
	list[0].State = "down"
	if change, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || !change.Changed() {
		t.Fatalf("changed state: change=%+v err=%v", change, err)
	}
	if change, err := s.ReplaceSlurmGPUNodes(c.ID, list[:1]); err != nil || !change.Changed() {
		t.Fatalf("shorter list: change=%+v err=%v", change, err)
	}
	names, err := s.SlurmGPUNodeNames(c.ID)
	if err != nil || len(names) != 1 || names[0] != "gpu-2" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if change, err := s.ReplaceSlurmGPUNodes(c.ID, nil); err != nil || !change.Changed() {
		t.Fatalf("empty list: change=%+v err=%v", change, err)
	}
	// Another cluster's list is untouched by this one's.
	other := newSlurmCluster(t, s, "other")
	if _, err := s.ReplaceSlurmGPUNodes(other.ID, list); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, list[:1]); err != nil {
		t.Fatal(err)
	}
	counts, err := s.SlurmGPUNodeCounts()
	if err != nil || counts[c.ID] != 1 || counts[other.ID] != 2 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	// The same node cannot be listed twice in one list.
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "a"}, {Name: "a"}}); err == nil {
		t.Fatal("a list with one node twice was stored")
	}
	if names, _ := s.SlurmGPUNodeNames(c.ID); len(names) != 1 || names[0] != "gpu-2" {
		t.Fatalf("a failed replacement changed the list: %v", names)
	}
}

func TestSlurmMissingNodesAreTheListedOnesWithoutAnOnlineAgent(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	other := newSlurmCluster(t, s, "other")
	for _, name := range []string{"gpu-1", "gpu-2", "gpu-3"} {
		if _, _, err := s.EnrollSlurmNode(c.Token, "demo", name); err != nil {
			t.Fatal(err)
		}
	}
	// gpu-1 is online, gpu-2 enrolled and offline, gpu-3 online, gpu-4 never enrolled.
	if err := s.DB().Model(&Node{}).Where("slurm_node IN ?", []string{"gpu-1", "gpu-3"}).Update("state", NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Model(&Node{}).Where("slurm_node = ?", "gpu-2").Update("state", NodeStateOffline).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "gpu-1", State: "idle", GPUs: 1}, {Name: "gpu-2", State: "drained", Reason: "Kill task failed", GPUs: 1},
		{Name: "gpu-4", State: "down", Reason: "Not responding", GPUs: 2}}); err != nil {
		t.Fatal(err)
	}
	// The other entry lists gpu-1 too, but its own node of that name never enrolled.
	if _, err := s.ReplaceSlurmGPUNodes(other.ID, []SlurmGPUNode{{Name: "gpu-1", State: "idle", GPUs: 1}}); err != nil {
		t.Fatal(err)
	}
	missing, err := s.SlurmMissingNodes()
	if err != nil {
		t.Fatal(err)
	}
	var gpu2 Node
	if err := s.DB().Where("slurm_cluster_id = ? AND slurm_node = ?", c.ID, "gpu-2").First(&gpu2).Error; err != nil {
		t.Fatal(err)
	}
	want := []SlurmMissingNode{
		{ClusterID: c.ID, Name: "gpu-2", State: "drained", Reason: "Kill task failed", Enrolled: true, NodeUUID: gpu2.UUID},
		{ClusterID: c.ID, Name: "gpu-4", State: "down", Reason: "Not responding", Enrolled: false},
		{ClusterID: other.ID, Name: "gpu-1", State: "idle", Enrolled: false},
	}
	if len(missing) != len(want) {
		t.Fatalf("missing = %+v", missing)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("missing[%d] = %+v, want %+v", i, missing[i], want[i])
		}
	}
}

// The reconciliation runs on every list of the clusters: it must be one
// indexed join, never a scan of the node table.
func TestSlurmMissingNodesQueryUsesTheNodeIndex(t *testing.T) {
	s := newTestStore(t)
	var plan []struct {
		Detail string
	}
	if err := s.DB().Raw(`EXPLAIN QUERY PLAN SELECT g.cluster_id AS cluster_id, g.name AS name, g.state AS state, g.reason AS reason, n.id IS NOT NULL AS enrolled
		FROM slurm_gpu_nodes g LEFT JOIN nodes n ON n.slurm_cluster_id > 0 AND n.slurm_cluster_id = g.cluster_id AND n.slurm_node = g.name
		WHERE n.id IS NULL OR n.state <> 'online' ORDER BY g.cluster_id, g.name`).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range plan {
		if strings.HasPrefix(p.Detail, "SEARCH n USING INDEX idx_node_slurm (slurm_cluster_id=? AND slurm_node=?)") {
			found = true
		}
		if strings.HasPrefix(p.Detail, "SCAN n") {
			t.Fatalf("the node table is scanned: %v", plan)
		}
	}
	if !found {
		t.Fatalf("the join does not use idx_node_slurm: %v", plan)
	}
}

// A node that joined through a Slurm entry is listed under it, and under the
// account the entry was made from, like a Kubernetes cluster's node.
func TestNodeSourceNamesTheSlurmClusterAndItsAccount(t *testing.T) {
	s := newTestStore(t)
	acct := &Provider{Name: "aliyun-main", Kind: "aliyun"}
	if err := s.DB().Create(acct).Error; err != nil {
		t.Fatal(err)
	}
	cloud, err := s.CreateSlurmCluster(SlurmCluster{Name: "ehpc", ProviderID: &acct.ID, ProviderClusterID: "ehpc-1"})
	if err != nil {
		t.Fatal(err)
	}
	lab := newSlurmCluster(t, s, "lab")
	cn, _, err := s.EnrollSlurmNode(cloud.Token, "demo", "cn000")
	if err != nil {
		t.Fatal(err)
	}
	ln, _, err := s.EnrollSlurmNode(lab.Token, "labslurm", "gpu01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNode("hand", HashToken("t"), "hand"); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, fleet := range []bool{false, true} {
		sources, err := s.nodeSources(nodes, fleet)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]NodeSource{
			cn.UUID: {ProviderID: acct.ID, ProviderName: "aliyun-main", ProviderKind: "aliyun", ClusterUUID: cloud.UUID, ClusterName: "ehpc", ClusterKind: NodeSourceSlurm},
			ln.UUID: {ClusterUUID: lab.UUID, ClusterName: "lab", ClusterKind: NodeSourceSlurm},
		}
		seen := 0
		for i := range nodes {
			got, joined := sources[nodes[i].ID]
			if w, ok := want[nodes[i].UUID]; ok {
				seen++
				if !joined || got != w {
					t.Fatalf("fleet=%v node %s source = %+v (joined %v), want %+v", fleet, nodes[i].Name, got, joined, w)
				}
			} else if joined {
				t.Fatalf("fleet=%v node %s has source %+v, it joined through none", fleet, nodes[i].Name, got)
			}
		}
		if seen != 2 {
			t.Fatalf("fleet=%v saw %d Slurm nodes", fleet, seen)
		}
	}
	counts, err := s.NodeCountsByProvider()
	if err != nil || len(counts) != 1 || counts[acct.ID] != 1 {
		t.Fatalf("node counts by account = %v, %v", counts, err)
	}
}

func TestOfflineSlurmNodesAreThoseTheirClusterNoLongerLists(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	empty := newSlurmCluster(t, s, "silent")
	long := time.Now().Add(-time.Hour)
	enroll := func(cluster *SlurmCluster, name, state string, seen *time.Time) {
		t.Helper()
		if _, _, err := s.EnrollSlurmNode(cluster.Token, "demo-"+cluster.Name, name); err != nil {
			t.Fatal(err)
		}
		if err := s.DB().Model(&Node{}).Where("slurm_cluster_id = ? AND slurm_node = ?", cluster.ID, name).Updates(map[string]any{"state": state, "last_seen_at": seen}).Error; err != nil {
			t.Fatal(err)
		}
	}
	recent := time.Now()
	enroll(c, "listed-gone", NodeStateOffline, &long)       // listed: stays
	enroll(c, "unlisted-gone", NodeStateOffline, &long)     // not listed, long gone: a candidate
	enroll(c, "no-gres", NodeStateOffline, &long)           // listed without a GPU GRES: still in the cluster, stays
	enroll(c, "unlisted-recent", NodeStateOffline, &recent) // gone a moment: not yet
	enroll(c, "unlisted-online", NodeStateOnline, &recent)  // online: never
	enroll(empty, "no-list", NodeStateOffline, &long)       // its entry never reported: condemns nothing
	// The cluster also has nodes without a GPU GRES (or in a hidden partition):
	// listed by name only, they are still in the cluster.
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "listed-gone", State: "idle", GPUs: 1},
		{Name: "no-gres", ListedOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNode("hand", HashToken("t"), "hand"); err != nil { // no entry at all
		t.Fatal(err)
	}
	got, err := s.OfflineSlurmNodes(time.Now().Add(-15 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, n := range got {
		names = append(names, n.SlurmNode)
	}
	if want := []string{"unlisted-gone"}; len(names) != 1 || names[0] != want[0] {
		t.Fatalf("candidates = %v, want %v", names, want)
	}
}

// Enrollment runs on every agent start, and the cluster's status page counts
// its nodes on every poll: none of those reads may scan the node table.
func TestSlurmNodeLookupsUseTheNodeIndex(t *testing.T) {
	s := newTestStore(t)
	for name, query := range map[string]string{
		"find the node of an entry by its Slurm name": "SELECT * FROM nodes WHERE " + slurmNodeIs,
		"count an entry's nodes":                      "SELECT count(*) FROM nodes WHERE slurm_cluster_id > 0 AND slurm_cluster_id = ?",
		"count every entry's nodes":                   "SELECT slurm_cluster_id, count(*) FROM nodes WHERE slurm_cluster_id > 0 GROUP BY slurm_cluster_id",
	} {
		var plan []struct{ Detail string }
		args := []any{}
		for range strings.Count(query, "?") {
			args = append(args, 1)
		}
		if err := s.DB().Raw("EXPLAIN QUERY PLAN "+query, args...).Scan(&plan).Error; err != nil {
			t.Fatal(err)
		}
		used := false
		for _, p := range plan {
			if strings.HasPrefix(p.Detail, "SCAN nodes") && !strings.Contains(p.Detail, "INDEX") {
				t.Errorf("%s scans the node table: %v", name, plan)
			}
			used = used || strings.Contains(p.Detail, "idx_node_slurm")
		}
		if !used {
			t.Errorf("%s does not use idx_node_slurm: %v", name, plan)
		}
	}
}

// A node of the cluster without a GPU GRES is kept by name, so the sweep knows
// it is still there, but it is no GPU node: not counted, not expected to have
// an agent, not missing.
func TestListedOnlySlurmNodesAreNoGPUNodes(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "gpu-1", State: "idle", GPUs: 1}, {Name: "cpu-1", ListedOnly: true}, {Name: "cpu-2", ListedOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if counts, err := s.SlurmGPUNodeCounts(); err != nil || counts[c.ID] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	if names, err := s.SlurmGPUNodeNames(c.ID); err != nil || len(names) != 1 || names[0] != "gpu-1" {
		t.Fatalf("names = %v, %v", names, err)
	}
	missing, err := s.SlurmMissingNodes(c.ID)
	if err != nil || len(missing) != 1 || missing[0].Name != "gpu-1" {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
}

// A new state or reason rewrites that row alone, and a list whose states are
// all as stored writes nothing; only a change of membership (a node coming or
// going, its GPU count, whether it is a GPU node) replaces the rows.
func TestReplaceSlurmGPUNodesUpdatesOnlyTheRowsWhoseStateChanged(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	list := []SlurmGPUNode{{Name: "a", State: "idle", GPUs: 1}, {Name: "b", State: "idle", GPUs: 1}, {Name: "c", ListedOnly: true}}
	if ch, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || !ch.Replaced {
		t.Fatalf("first = %+v, %v", ch, err)
	}
	ids := func() map[string]uint {
		var rows []SlurmGPUNode
		if err := s.DB().Order("id").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		out := map[string]uint{}
		for _, r := range rows {
			out[r.Name] = r.ID
		}
		return out
	}
	first := ids()
	list[1].State, list[1].Reason = "drained", "Kill task failed"
	ch, err := s.ReplaceSlurmGPUNodes(c.ID, list)
	if err != nil || ch.Replaced || ch.StatesUpdated != 1 {
		t.Fatalf("one state changed = %+v, %v", ch, err)
	}
	if got := ids(); got["a"] != first["a"] || got["b"] != first["b"] || got["c"] != first["c"] {
		t.Fatalf("rows were replaced for a state change: %v -> %v", first, got)
	}
	var b SlurmGPUNode
	if err := s.DB().Where("name = ?", "b").First(&b).Error; err != nil || b.State != "drained" || b.Reason != "Kill task failed" {
		t.Fatalf("row b = %+v, %v", b, err)
	}
	if ch, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || ch.Changed() {
		t.Fatalf("the same list again = %+v, %v", ch, err)
	}
	// A reason cleared is a change too (no zero-value skipping).
	list[1].State, list[1].Reason = "idle", ""
	if ch, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || ch.StatesUpdated != 1 {
		t.Fatalf("state back to idle = %+v, %v", ch, err)
	}
	if err := s.DB().Where("name = ?", "b").First(&b).Error; err != nil || b.State != "idle" || b.Reason != "" {
		t.Fatalf("row b = %+v, %v", b, err)
	}
	// Membership: the GPU count, the GPU flag, or a node more.
	list[0].GPUs = 2
	if ch, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || !ch.Replaced {
		t.Fatalf("GPU count changed = %+v, %v", ch, err)
	}
	list[2] = SlurmGPUNode{Name: "c", State: "idle", GPUs: 1}
	if ch, err := s.ReplaceSlurmGPUNodes(c.ID, list); err != nil || !ch.Replaced {
		t.Fatalf("a listed-only node became a GPU node = %+v, %v", ch, err)
	}
}

// The entries of an account's HPC clusters are read for what a listing shows
// (their cluster, uuid and name), by the account, and nothing else of them,
// the enrollment token above all.
func TestSlurmClustersFromProviderReadsTheEntriesOfOneAccountWithoutTheirToken(t *testing.T) {
	s := newTestStore(t)
	mine, other := uint(7), uint(8)
	a, err := s.CreateSlurmCluster(SlurmCluster{Name: "ehpc-a", ProviderID: &mine, ProviderClusterID: "ehpc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSlurmCluster(SlurmCluster{Name: "ehpc-b", ProviderID: &other, ProviderClusterID: "ehpc-1"}); err != nil {
		t.Fatal(err)
	}
	newSlurmCluster(t, s, "by-hand")
	got, err := s.SlurmClustersFromProvider(mine)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["ehpc-1"] != (SlurmClusterSource{ProviderClusterID: "ehpc-1", UUID: a.UUID, Name: "ehpc-a"}) {
		t.Fatalf("entries = %+v", got)
	}
	if none, err := s.SlurmClustersFromProvider(99); err != nil || len(none) != 0 {
		t.Fatalf("an account with no entry = %+v, %v", none, err)
	}
}

// Nodes that enrolled since a time and that no reported list names are what
// the entry's GPU node count adds to the list: a list that came after a node
// enrolled and does not name it has left it out, so it is no longer counted.
func TestSlurmUnlistedNodeCountsAreTheRecentNodesNoListNames(t *testing.T) {
	s := newTestStore(t)
	c := newSlurmCluster(t, s, "hpc")
	other := newSlurmCluster(t, s, "other")
	for _, name := range []string{"gpu-1", "gpu-2", "gpu-3", "gpu-4"} {
		if _, _, err := s.EnrollSlurmNode(c.Token, "demo", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.EnrollSlurmNode(other.Token, "demo2", "gpu-1"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	age := func(name string, d time.Duration) {
		t.Helper()
		if err := s.DB().Model(&Node{}).Where("slurm_cluster_id = ? AND slurm_node = ?", c.ID, name).Update("created_at", now.Add(-d)).Error; err != nil {
			t.Fatal(err)
		}
	}
	age("gpu-3", time.Hour)
	age("gpu-4", time.Hour)
	// gpu-1 is listed as a GPU node, gpu-4 as a node without a GPU; gpu-2 (new)
	// and gpu-3 (old) are named by no list.
	if _, err := s.ReplaceSlurmGPUNodes(c.ID, []SlurmGPUNode{{Name: "gpu-1", State: "idle", GPUs: 1}, {Name: "gpu-4", ListedOnly: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.SlurmUnlistedNodeCounts(now.Add(-10 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got[c.ID] != 1 || got[other.ID] != 1 {
		t.Fatalf("unlisted recent nodes = %v, want gpu-2 of one entry and the other entry's own node", got)
	}
	// With the cut-off before every node, the old unlisted one counts too.
	got, _ = s.SlurmUnlistedNodeCounts(now.Add(-2 * time.Hour))
	if got[c.ID] != 2 {
		t.Fatalf("unlisted nodes since long ago = %v", got)
	}
}

// Online counts per entry, and when its agents were last seen: the age of a
// list no agent online can update.
func TestSlurmClusterOnlineCountsAndLastSeen(t *testing.T) {
	s := newTestStore(t)
	c, err := s.CreateSlurmCluster(SlurmCluster{Name: "hpc"})
	if err != nil {
		t.Fatal(err)
	}
	older, newer := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	for name, seen := range map[string]time.Time{"gpu-1": older, "gpu-2": newer} {
		n, _, err := s.EnrollSlurmNode(c.Token, "demo", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DB().Model(&Node{}).Where("id = ?", n.ID).Updates(map[string]any{"state": NodeStateOffline, "last_seen_at": seen}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DB().Model(&Node{}).Where("slurm_node = ?", "gpu-1").Update("state", NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	counts, err := s.SlurmClusterNodeCounts()
	if err != nil || counts[c.ID] != (SlurmNodeCounts{Enrolled: 2, Online: 1}) {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	seen, err := s.SlurmClustersLastSeen(c.ID, c.ID+1)
	if err != nil || !seen[c.ID].Equal(newer) || len(seen) != 1 {
		t.Fatalf("last seen = %v, %v; want %v for the entry and nothing for one without nodes", seen, err, newer)
	}
}
