package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// runtimeNode is one entry of a deployment's runtime_nodes: where a replica
// runs now, as the wire states it. These are the test's own types on purpose -
// they decode the JSON rather than the Go DTO, so the assertions state what a
// client sees and the file compiles against a build that lacks the fields, which
// is what lets one test be run before and after the fix.
type runtimeNode struct {
	NodeUUID  string `json:"node_uuid"`
	NodeName  string `json:"node_name"`
	Instances int    `json:"instances"`
}

type runtimeRow struct {
	Name             string        `json:"name"`
	NodeUUID         string        `json:"node_uuid"`
	NodeName         string        `json:"node_name"`
	RuntimeNodes     []runtimeNode `json:"runtime_nodes"`
	RuntimeNodeNames []string      `json:"runtime_node_names"`
}

// The 「计算节点」 column of the deployments list must show where the replicas
// run, and the GPU inventory agrees with it because both read the instance
// rows. These tests pin the three ways the requested node and the node actually
// used come apart on a live fleet (GWKS-120):
//
//	(a) automatic placement put the replica on another node than the one kept
//	    as the placement target,
//	(b) a spread deployment runs its replicas on several nodes at once, so no
//	    single node names them all,
//	(c) a replica failed on a node and was retried, leaving the row that
//	    actually ran it on a node the deployment does not name.
type runtimeNodeFixture struct {
	st      *store.Store
	handler *apiHandlers
	modelID uint
	// target is the node the deployments name as their placement target.
	target uint
	// elsewhere is a node of the same fleet that no deployment targets.
	elsewhere uint
}

func newRuntimeNodeFixture(t *testing.T) runtimeNodeFixture {
	t.Helper()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node := func(name string) *store.Node {
		n, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetNodeState(n.UUID, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// target is the node an operator asked for; elsewhere is where the platform
	// actually put work, which is the case the bug hid.
	target, elsewhere := node("172-24-0-212"), node("172-20-1-163")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/rt", Revision: "main", Name: "org/rt",
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	if err != nil {
		t.Fatal(err)
	}
	return runtimeNodeFixture{st: st, handler: &apiHandlers{d: Deps{Store: st}}, modelID: model.ID,
		target: target.ID, elsewhere: elsewhere.ID}
}

// deploy creates a deployment naming target as its placement target; instance
// puts one instance row on a node. They are the smallest writes the list reads.
func (f runtimeNodeFixture) deploy(t *testing.T, name string, replicas int) *store.Deployment {
	t.Helper()
	d, err := f.st.CreateDeployment(&store.Deployment{
		Name: name, ModelID: f.modelID, NodeID: f.target, Replicas: replicas, PlacementStrategy: store.PlacementSpread,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f runtimeNodeFixture) instance(t *testing.T, dep *store.Deployment, nodeID uint, uuid, state string) {
	t.Helper()
	if _, err := f.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: nodeID, State: state,
		Image: "example/engine:1", GPUIndexes: "[0]", Ports: "[8000]"}); err != nil {
		t.Fatal(err)
	}
}

func decodeRuntimeRow(t *testing.T, body []byte) runtimeRow {
	t.Helper()
	var row runtimeRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

// row is the deployment's list row, read through GET /deployments?page=1 - the
// endpoint the table calls. Reading the row rather than the DTO keeps the test
// on the surface the bug was reported against.
func (f runtimeNodeFixture) row(t *testing.T, name string) runtimeRow {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.listDeployments(rec, httptest.NewRequest(http.MethodGet, "/api/v1/deployments?page=1&page_size=50", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /deployments = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, raw := range page.Items {
		if row := decodeRuntimeRow(t, raw); row.Name == name {
			return row
		}
	}
	t.Fatalf("deployment %q is not on the list page", name)
	return runtimeRow{}
}

// checkRuntimeNodes asserts both halves at once: the placement target keeps
// naming the node asked for, and runtime_nodes reports the nodes in use.
func checkRuntimeNodes(t *testing.T, row runtimeRow, wantTarget string, wantNodes []runtimeNode) {
	t.Helper()
	if row.NodeName != wantTarget {
		t.Errorf("node_name = %q, want the placement target %q to keep its meaning", row.NodeName, wantTarget)
	}
	if !reflect.DeepEqual(row.RuntimeNodes, wantNodes) {
		t.Errorf("runtime_nodes = %+v, want %+v (where the replicas actually run)", row.RuntimeNodes, wantNodes)
	}
	names := make([]string, 0, len(wantNodes))
	for _, node := range wantNodes {
		names = append(names, node.NodeName)
	}
	if len(wantNodes) == 0 {
		names = nil
	}
	if !reflect.DeepEqual(row.RuntimeNodeNames, names) {
		t.Errorf("runtime_node_names = %v, want %v (the names of runtime_nodes)", row.RuntimeNodeNames, names)
	}
}

// (a) The platform placed the replica somewhere other than the node the
// deployment still names. The row must report where it runs.
func TestDeploymentRuntimeNodesFollowAutomaticPlacement(t *testing.T) {
	f := newRuntimeNodeFixture(t)
	dep := f.deploy(t, "auto-placed", 1)
	f.instance(t, dep, f.elsewhere, "inst-elsewhere", proto.InstRunning)
	checkRuntimeNodes(t, f.row(t, "auto-placed"), "172-24-0-212", []runtimeNode{
		{NodeUUID: "172-20-1-163", NodeName: "172-20-1-163", Instances: 1},
	})
}

// (b) Two replicas spread across two nodes: the row must report both, not the
// one the deployment is keyed by and not a winner-takes-all single node.
func TestDeploymentRuntimeNodesListEveryNodeOfASpreadDeployment(t *testing.T) {
	f := newRuntimeNodeFixture(t)
	dep := f.deploy(t, "spread", 2)
	f.instance(t, dep, f.target, "inst-spread-target", proto.InstRunning)
	f.instance(t, dep, f.elsewhere, "inst-spread-elsewhere", proto.InstRunning)
	checkRuntimeNodes(t, f.row(t, "spread"), "172-24-0-212", []runtimeNode{
		{NodeUUID: "172-20-1-163", NodeName: "172-20-1-163", Instances: 1},
		{NodeUUID: "172-24-0-212", NodeName: "172-24-0-212", Instances: 1},
	})
}

// (c) The replica failed on the node the platform moved it to. That node is
// where an operator goes for its logs, so a deployment with no running replica
// still names the node instead of going blank and leaving the target's name to
// send the operator to a machine the replica never ran on.
func TestDeploymentRuntimeNodesKeepTheNodeFailedReplicasMovedTo(t *testing.T) {
	f := newRuntimeNodeFixture(t)
	dep := f.deploy(t, "failed-away", 1)
	f.instance(t, dep, f.elsewhere, "inst-failed-away", proto.InstFailed)
	checkRuntimeNodes(t, f.row(t, "failed-away"), "172-24-0-212", []runtimeNode{
		{NodeUUID: "172-20-1-163", NodeName: "172-20-1-163", Instances: 1},
	})
}

// A replica retried elsewhere leaves a stopped row on the node it left. That row
// records a container that is gone, so it must not add the old node back.
func TestDeploymentRuntimeNodesIgnoreTheNodeAReplicaLeft(t *testing.T) {
	f := newRuntimeNodeFixture(t)
	dep := f.deploy(t, "retried", 1)
	f.instance(t, dep, f.target, "inst-left", proto.InstStopped)
	f.instance(t, dep, f.elsewhere, "inst-retried", proto.InstRunning)
	checkRuntimeNodes(t, f.row(t, "retried"), "172-24-0-212", []runtimeNode{
		{NodeUUID: "172-20-1-163", NodeName: "172-20-1-163", Instances: 1},
	})
}

// Nothing has been placed yet: the deployment names only its placement target,
// and runtime_nodes stays off the row rather than claiming a node.
func TestDeploymentRuntimeNodesAreAbsentBeforePlacement(t *testing.T) {
	f := newRuntimeNodeFixture(t)
	f.deploy(t, "unplaced", 1)
	checkRuntimeNodes(t, f.row(t, "unplaced"), "172-24-0-212", nil)
}

// The row and the object must describe the nodes identically, or the table and
// the drawer behind it would disagree - the same bug in another place.
func TestDeploymentRowAndObjectAgreeOnRuntimeNodes(t *testing.T) {
	f := newRuntimeNodeFixture(t)
	dep := f.deploy(t, "agree", 2)
	f.instance(t, dep, f.target, "inst-agree-target", proto.InstRunning)
	f.instance(t, dep, f.elsewhere, "inst-agree-elsewhere", proto.InstStarting)
	stored, err := f.st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(f.handler.deploymentDTO(stored, false))
	if err != nil {
		t.Fatal(err)
	}
	detail, row := decodeRuntimeRow(t, body), f.row(t, "agree")
	if !reflect.DeepEqual(detail.RuntimeNodes, row.RuntimeNodes) {
		t.Errorf("detail runtime_nodes = %+v, row runtime_nodes = %+v", detail.RuntimeNodes, row.RuntimeNodes)
	}
	if !reflect.DeepEqual(detail.RuntimeNodeNames, row.RuntimeNodeNames) {
		t.Errorf("detail runtime_node_names = %v, row = %v", detail.RuntimeNodeNames, row.RuntimeNodeNames)
	}
}
