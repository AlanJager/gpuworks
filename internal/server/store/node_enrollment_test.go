package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// capGuard lets at most max nodes into the fleet, the way a site cap would,
// and records the capacity it was shown.
type capGuard struct {
	max  int
	seen atomic.Int64
	last atomic.Pointer[FleetCapacity]
}

func (g *capGuard) guard(c FleetCapacity) *messages.ProductError {
	g.seen.Add(1)
	g.last.Store(&c)
	if c.Nodes >= g.max {
		return messages.NewProductError("SITE_NODE_CAP", "the site allows only a few nodes", map[string]any{"max": g.max, "nodes": c.Nodes})
	}
	return nil
}

func openDiskStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "enroll.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func assertRefused(t *testing.T, err error, code string) {
	t.Helper()
	if !errors.Is(err, ErrNodeEnrollmentRefused) {
		t.Fatalf("err = %v, want ErrNodeEnrollmentRefused", err)
	}
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != code {
		t.Fatalf("err = %v, want the product error %s", err, code)
	}
}

func countNodes(t *testing.T, st *Store) int64 {
	t.Helper()
	var n int64
	if err := st.db.Model(&Node{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// With no guard, or a nil one, every insert path works as before and
// counts nothing.
func TestNodeInsertsWithoutAGuardAreNeverRefused(t *testing.T) {
	st := openKubeStore(t)
	st.AddNodeEnrollmentGuard(nil)
	if err := st.CheckNodeEnrollment(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateNodePending("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("legacy", HashToken("t"), "h"); err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateKubeCluster(KubeCluster{Name: "k", Namespace: "gpuworks-system", IdleReturnSeconds: KubeDefaultIdleReturnSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(c.Token, "uid", "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if got := countNodes(t, st); got != 3 {
		t.Fatalf("nodes = %d, want 3", got)
	}
}

// Each of the four insert paths asks the guard inside its transaction and
// creates nothing when refused; the refusal carries the guard's code.
func TestEveryNodeInsertPathAsksTheGuard(t *testing.T) {
	st := openKubeStore(t)
	kube, err := st.CreateKubeCluster(KubeCluster{Name: "k", Namespace: "gpuworks-system", IdleReturnSeconds: KubeDefaultIdleReturnSeconds})
	if err != nil {
		t.Fatal(err)
	}
	g := &capGuard{max: 1}
	st.AddNodeEnrollmentGuard(g.guard)
	if _, _, err := st.CreateNodePending("first"); err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)

	_, _, err = st.CreateNodePending("second")
	assertRefused(t, err, "SITE_NODE_CAP")
	_, err = st.RegisterNode("legacy", HashToken("t"), "h")
	assertRefused(t, err, "SITE_NODE_CAP")
	_, _, err = st.EnrollKubeNode(kube.Token, "uid", "gpu-1")
	assertRefused(t, err, "SITE_NODE_CAP")
	slurm := newSlurmCluster(t, st, "hpc")
	_, _, err = st.EnrollSlurmNode(slurm.Token, "demo", "gpu-1")
	assertRefused(t, err, "SITE_NODE_CAP")
	assertRefused(t, st.CheckNodeEnrollment(), "SITE_NODE_CAP")

	if got := countNodes(t, st); got != 1 {
		t.Fatalf("nodes = %d after four refusals, want 1", got)
	}
	// The refused Slurm enrollment did not bind the token to the cluster.
	if again, err := st.SlurmClusterByUUID(slurm.UUID); err != nil || again.SlurmClusterName != "" {
		t.Fatalf("slurm cluster after a refused enrollment = %+v err=%v", again, err)
	}
	select {
	case ev := <-sub.C():
		t.Fatalf("a refused insert was published: %+v", ev)
	default:
	}
	// The refused kube enrollment did not bind the token either.
	again, err := st.KubeClusterByUUID(kube.UUID)
	if err != nil || again.ClusterUID != "" {
		t.Fatalf("cluster after a refused enrollment = %+v err=%v", again, err)
	}
	var pe *messages.ProductError
	_, _, err = st.CreateNodePending("third")
	if !errors.As(err, &pe) || pe.Params()["max"] != 1 || !strings.Contains(err.Error(), "the site allows only a few nodes") {
		t.Fatalf("refusal = %v params=%v", err, pe.Params())
	}
}

// A Kubernetes node that enrolled before is never refused: re-enrolling
// returns its row before any insert is attempted.
func TestReEnrollingAKubeNodeIsNeverRefused(t *testing.T) {
	st := openKubeStore(t)
	kube, err := st.CreateKubeCluster(KubeCluster{Name: "k", Namespace: "gpuworks-system", IdleReturnSeconds: KubeDefaultIdleReturnSeconds})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := st.EnrollKubeNode(kube.Token, "uid", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	g := &capGuard{max: 0}
	st.AddNodeEnrollmentGuard(g.guard)
	again, _, err := st.EnrollKubeNode(kube.Token, "uid", "gpu-1")
	if err != nil || again.UUID != first.UUID {
		t.Fatalf("re-enroll = %+v err=%v, want the same node back", again, err)
	}
	if g.seen.Load() != 0 {
		t.Fatal("re-enrolling an existing node asked the guard")
	}
	_, _, err = st.EnrollKubeNode(kube.Token, "uid", "gpu-2")
	assertRefused(t, err, "SITE_NODE_CAP")
}

// A Slurm node that enrolled before is never refused: re-enrolling returns
// its row before any insert is attempted. A new one is refused, and a
// taken insert tells the fleet's watchers.
func TestReEnrollingASlurmNodeIsNeverRefused(t *testing.T) {
	st := openKubeStore(t)
	slurm := newSlurmCluster(t, st, "hpc")
	sub := st.Bus.Subscribe(16, NodeFleetTopic)
	t.Cleanup(sub.Close)
	first, _, err := st.EnrollSlurmNode(slurm.Token, "demo", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.C():
		if change, ok := NodeFleetChangeOf(ev); !ok || change.NodeID != first.ID || change.Kind != NodeInserted {
			t.Fatalf("the insert was published as %+v", ev)
		}
	default:
		t.Fatal("a Slurm node's insert was not published to the fleet's watchers")
	}
	g := &capGuard{max: 0}
	st.AddNodeEnrollmentGuard(g.guard)
	again, _, err := st.EnrollSlurmNode(slurm.Token, "demo", "gpu-1")
	if err != nil || again.UUID != first.UUID || again.Token != first.Token {
		t.Fatalf("re-enroll = %+v err=%v, want the same node back", again, err)
	}
	if g.seen.Load() != 0 {
		t.Fatal("re-enrolling an existing node asked the guard")
	}
	_, _, err = st.EnrollSlurmNode(slurm.Token, "demo", "gpu-2")
	assertRefused(t, err, "SITE_NODE_CAP")
	if got := countNodes(t, st); got != 1 {
		t.Fatalf("nodes = %d, want the one that enrolled", got)
	}
}

// A tripwire, not a proof: it reads this package's source for the common
// ways of writing a node insert (Create, Save and the like on a variable
// named node, n or nodes or on a Node literal, and INSERT INTO nodes) and
// fails on one outside insertNodeTx. A node created under another name, or
// through a helper, passes it. What guarantees that an insert asks the
// guards is the behaviour test of each path (TestEveryNodeInsertPathAsksTheGuard);
// this only makes the usual mistake fail early.
func TestNoNodeIsCreatedBesideTheGuardedInsert(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// A create whose argument is a node: a variable or parameter the
	// package names node, n or nodes, or a Node literal.
	creates := regexp.MustCompile(`\.(Create|CreateInBatches|Save|FirstOrCreate)\(\s*&?(node|n|nodes|\[\]Node\{|Node\{)\b`)
	insertsRow := regexp.MustCompile(`(?i)INSERT\s+(OR\s+\w+\s+)?INTO\s+nodes\b`)
	var found []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if creates.MatchString(line) || insertsRow.MatchString(line) {
				found = append(found, fmt.Sprintf("%s:%d: %s", file, i+1, strings.TrimSpace(line)))
			}
		}
	}
	// What the pattern finds that is not a node row, and the guarded insert.
	allowed := map[string]bool{
		"node_enrollment.go: return tx.Create(n).Error":                    true, // insertNodeTx itself
		"network_objects.go: if err := s.db.Create(n).Error; err != nil {": true, // a Network named n
	}
	for _, hit := range found {
		file, rest, _ := strings.Cut(hit, ":")
		_, line, _ := strings.Cut(rest, ": ")
		if !allowed[file+": "+line] {
			t.Errorf("%s creates a node row outside insertNodeTx, so the enrollment guards are not asked", hit)
		}
	}
	if len(found) < 2 {
		t.Fatalf("the pattern found %d creates; it no longer sees insertNodeTx's own, so it proves nothing", len(found))
	}
}

// Guards run in registration order, the first refusal wins, and a refusal
// without a code is shown as NODE_ENROLLMENT_REFUSED.
func TestEnrollmentGuardsFirstRefusalWins(t *testing.T) {
	st := newTestStore(t)
	var order []string
	st.AddNodeEnrollmentGuard(func(FleetCapacity) *messages.ProductError { order = append(order, "allow"); return nil })
	st.AddNodeEnrollmentGuard(func(FleetCapacity) *messages.ProductError {
		order = append(order, "uncoded")
		return messages.NewProductError("", "", map[string]any{"why": "freeze"})
	})
	st.AddNodeEnrollmentGuard(func(FleetCapacity) *messages.ProductError { order = append(order, "never"); return nil })
	_, _, err := st.CreateNodePending("n")
	assertRefused(t, err, messages.ErrorNodeEnrollmentRefused)
	var pe *messages.ProductError
	errors.As(err, &pe)
	if pe.Params()["why"] != "freeze" || pe.Error() == "" {
		t.Fatalf("uncoded refusal = %q %v", pe.Error(), pe.Params())
	}
	if strings.Join(order, ",") != "allow,uncoded" {
		t.Fatalf("guards ran %v", order)
	}
}

// The guard sees the fleet before the new node: every node, the nodes
// with a card of their own and their own cards. A card handed to a VM is
// not the platform's and is not counted.
func TestFleetCapacityCountsOnlyPlatformUsableCards(t *testing.T) {
	st := newTestStore(t)
	g := &capGuard{max: 100}
	st.AddNodeEnrollmentGuard(g.guard)
	for _, uuid := range []string{"cards", "vm", "empty"} {
		if _, err := st.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	ok := true
	report := func(uuid string, cards ...proto.GPUInfo) {
		t.Helper()
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
			t.Fatal(err)
		}
	}
	report("cards",
		proto.GPUInfo{Index: 0, UUID: "GPU-0", Vendor: "nvidia", PCIeBDF: "0000:01:00.0", Healthy: true},
		proto.GPUInfo{Index: 1, UUID: "GPU-1", Vendor: "nvidia", PCIeBDF: "0000:02:00.0", Healthy: true})
	report("vm", proto.GPUInfo{Index: 0, Vendor: "nvidia", PCIeBDF: "0000:03:00.0", Ownership: accelerator.CardPassedThrough})
	if _, _, err := st.CreateNodePending("next"); err != nil {
		t.Fatal(err)
	}
	if got := *g.last.Load(); got != (FleetCapacity{Nodes: 3, NodesWithCards: 1, OwnedCards: 2}) {
		t.Fatalf("capacity = %+v, want 3 nodes, 1 with cards, 2 owned cards", got)
	}
}

// Concurrent inserts on every path cannot both take the last place: the
// count and the insert share the writer's transaction.
func TestConcurrentNodeInsertsCannotOvershootTheGuard(t *testing.T) {
	for _, path := range []string{"pending", "register", "kube"} {
		t.Run(path, func(t *testing.T) {
			st := openDiskStore(t)
			kube, err := st.CreateKubeCluster(KubeCluster{Name: "k", Namespace: "gpuworks-system", IdleReturnSeconds: KubeDefaultIdleReturnSeconds})
			if err != nil {
				t.Fatal(err)
			}
			const limit, callers = 3, 12
			g := &capGuard{max: limit}
			st.AddNodeEnrollmentGuard(g.guard)
			var wg sync.WaitGroup
			var admitted, refused atomic.Int64
			for i := range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var err error
					switch path {
					case "pending":
						_, _, err = st.CreateNodePending(fmt.Sprintf("n%d", i))
					case "register":
						_, err = st.RegisterNode(fmt.Sprintf("u%d", i), HashToken(fmt.Sprint(i)), "h")
					case "kube":
						_, _, err = st.EnrollKubeNode(kube.Token, "uid", fmt.Sprintf("gpu-%d", i))
					}
					switch {
					case err == nil:
						admitted.Add(1)
					case errors.Is(err, ErrNodeEnrollmentRefused):
						refused.Add(1)
					default:
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if admitted.Load() != limit || refused.Load() != callers-limit || countNodes(t, st) != limit {
				t.Fatalf("admitted %d refused %d rows %d, want exactly %d admitted", admitted.Load(), refused.Load(), countNodes(t, st), limit)
			}
		})
	}
}

// The aggregate is one scan of nodes: no temporary b-tree, no join.
func TestFleetCapacityQueryPlanHasNoSort(t *testing.T) {
	st := newTestStore(t)
	var plan []struct{ Detail string }
	if err := st.db.Raw("EXPLAIN QUERY PLAN " + fleetCapacitySQL).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, p := range plan {
		details = append(details, p.Detail)
	}
	joined := strings.Join(details, "; ")
	if strings.Contains(joined, "TEMP B-TREE") || !strings.Contains(joined, "nodes") || strings.Contains(joined, "gpus") {
		t.Fatalf("plan = %s", joined)
	}
}

// The pre-check counts on the read pool and refuses like the insert.
func TestCheckNodeEnrollmentReadsTheFleet(t *testing.T) {
	st := openDiskStore(t)
	g := &capGuard{max: 2}
	st.AddNodeEnrollmentGuard(g.guard)
	for i := range 2 {
		if err := st.CheckNodeEnrollment(); err != nil {
			t.Fatalf("pre-check before node %d: %v", i, err)
		}
		if _, _, err := st.CreateNodePending(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	assertRefused(t, st.CheckNodeEnrollment(), "SITE_NODE_CAP")
	if got := g.last.Load().Nodes; got != 2 {
		t.Fatalf("pre-check saw %d nodes", got)
	}
}
