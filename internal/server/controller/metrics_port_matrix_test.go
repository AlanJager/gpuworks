package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// metricsEnv is a small fleet of two-card nodes sharing one model, with a
// controller over it, for the launch shapes api.metrics.port decides: one
// instance, a multi-node unit, several instances on the same node.
type metricsEnv struct {
	st    *store.Store
	hub   *distributedTestHub
	ctrl  *Controller
	nodes []*store.Node
	model *store.Model
}

func newMetricsEnv(t *testing.T, nodeCount int) *metricsEnv {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	st, err := store.Open("file:controller_metrics_matrix_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/metrics-" + name, Revision: "main", Name: "metrics-" + name,
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &metricsEnv{st: st, model: model}
	online := map[string]bool{}
	for index := 0; index < nodeCount; index++ {
		uuid := fmt.Sprintf("metrics-%s-n%d", name, index)
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.1.0.%d", index+10))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		for card := 0; card < 4; card++ {
			if err := st.DB().Create(&store.GPU{
				NodeID: node.ID, Index: card, UUID: fmt.Sprintf("%s-gpu%d", uuid, card), Name: "NVIDIA H100",
				Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/metrics"}); err != nil {
			t.Fatal(err)
		}
		e.nodes = append(e.nodes, node)
		online[node.UUID] = true
	}
	for _, engine := range []string{"vllm", "sglang", "callable"} {
		if _, err := st.UpsertImage(&store.Image{Name: engine, Engine: engine, Framework: "cuda", ImageRef: "catalog/metrics-" + engine + ":v1", Source: "official", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	e.hub = &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}
	e.ctrl = New(st, e.hub, nil)
	e.ctrl.SetRetryPolicy(0, 3)
	return e
}

// metricsSnapshot is the deployment's template snapshot: the engine's launch
// arguments, and the api.metrics declaration when metrics is not nil.
func metricsSnapshot(engine string, metrics map[string]any, referenceMetricsPort bool) map[string]any {
	args := []string{"--max-model-len", "512"}
	snapshot := map[string]any{}
	if engine == "callable" {
		args = []string{"--serve", "--port", "${GPUWORKS_API_PORT}"}
		snapshot["entrypoint"] = []string{"/opt/launcher"}
	}
	if referenceMetricsPort {
		args = append(args, "--metrics-listen", "${GPUWORKS_METRICS_PORT}")
	}
	snapshot["extra_args"] = args
	if metrics != nil {
		snapshot["api"] = map[string]any{"metrics": metrics}
	}
	return snapshot
}

// deployment creates a deployment of the engine on the nodes with the snapshot;
// more than one node makes it one multi-node unit across all of them.
func (e *metricsEnv) deployment(t *testing.T, name, engine string, snapshot map[string]any, nodes ...*store.Node) *store.Deployment {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{
		Name: name, ModelID: e.model.ID, NodeID: nodes[0].ID, ServedName: name, Engine: engine, State: "active",
		Image: "catalog/metrics-" + engine + ":v1", CatalogSelectionSnapshot: string(raw),
	}
	switch {
	case len(nodes) > 1:
		ids := make([]uint, 0, len(nodes))
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		encoded, _ := json.Marshal(ids)
		dep.NodeIDs, dep.NNodes = string(encoded), len(nodes)
		dep.DistGroup, dep.MasterAddr, dep.MasterPort = name+"-group", "10.1.0.10", 29500
		dep.Placement = `{"tp":4,"pp":2}`
	default:
		dep.QuickParams = `{"max_model_len":512}`
		dep.Placement = `{"tp":1}`
	}
	created, err := createTestDeployment(t, e.st, dep)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (e *metricsEnv) reconcile(t *testing.T, id uint, passes int) []store.Instance {
	t.Helper()
	for pass := 0; pass < passes; pass++ {
		if err := e.ctrl.reconcileDeployment(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	instances, err := e.st.InstancesForDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	return instances
}

// launchTokens is the command line the instance is started with: an engine
// that keeps its command in the image entrypoint states it there.
func launchTokens(spec proto.InstanceDesiredState) []string {
	return append(append([]string{}, spec.Entrypoint...), spec.Argv...)
}

// rawDesired is the stored desired state as the agent receives it: the wire
// keys, not the Go struct, because the contract is the absence of a key.
func rawDesired(t *testing.T, inst store.Instance) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &raw); err != nil {
		t.Fatalf("desired state %q: %v", inst.DesiredStateJSON, err)
	}
	return raw
}

// A deployment whose template does not declare api.metrics.port: separate
// launches exactly as it did before the field existed, for every engine and
// every launch shape: one API port reserved and held, no metrics port in the
// desired state (not even a zero), and a launch command that names no second
// port. Declaring port: api, or only an endpoint, is the same thing.
func TestWithoutADeclarationEveryLaunchShapeKeepsOnePortAndNoMetricsKey(t *testing.T) {
	declarations := map[string]map[string]any{
		"no api.metrics":   nil,
		"port api":         {"port": "api"},
		"endpoint changed": {"endpoint": "/m"},
	}
	shapes := []struct {
		name   string
		engine string
		nodes  int
	}{
		{"vllm single node", "vllm", 1},
		{"sglang single node", "sglang", 1},
		{"callable single node", "callable", 1},
		{"vllm multi node", "vllm", 2},
		{"sglang multi node", "sglang", 2},
	}
	for _, shape := range shapes {
		for declName, metrics := range declarations {
			t.Run(shape.name+"/"+declName, func(t *testing.T) {
				e := newMetricsEnv(t, 2)
				dep := e.deployment(t, "plain", shape.engine, metricsSnapshot(shape.engine, metrics, false), e.nodes[:shape.nodes]...)
				instances := e.reconcile(t, dep.ID, 2)
				if len(instances) != shape.nodes {
					t.Fatalf("instances = %d, want %d", len(instances), shape.nodes)
				}
				for _, inst := range instances {
					raw := rawDesired(t, inst)
					if _, present := raw["metrics_port"]; present {
						t.Fatalf("rank %d desired state carries metrics_port: %s", inst.NodeRank, inst.DesiredStateJSON)
					}
					spec := desiredOf(t, inst)
					wantPorts := []int{spec.APIPort}
					if inst.Headless {
						wantPorts = []int{}
					}
					if spec.MetricsPort != 0 || !slices.Equal(spec.Ports, wantPorts) || inst.Ports != jsonInts(wantPorts) {
						t.Fatalf("rank %d: api %d metrics %d ports %v instance ports %s", inst.NodeRank, spec.APIPort, spec.MetricsPort, spec.Ports, inst.Ports)
					}
					if strings.Contains(inst.DesiredStateJSON, "metrics-listen") {
						t.Fatalf("rank %d command names a metrics port: %s", inst.NodeRank, inst.DesiredStateJSON)
					}
				}
			})
		}
	}
}

// Stating the API port as the metrics port (api.metrics.port: api, which is
// what a template without the declaration means) changes nothing about the
// launch: the desired state of the same deployment is byte for byte the one a
// template with no api.metrics gets, apart from the identity of the rows and
// the stored contract that holds the declaration itself. (A template that
// moves the endpoint or maps families changes the observation, which is the
// point of those fields, and is covered by the key-absence test above.)
func TestAMetricsDeclarationThatIsNotSeparateLeavesTheDesiredStateByteIdentical(t *testing.T) {
	desired := func(t *testing.T, metrics map[string]any) string {
		e := newMetricsEnv(t, 2)
		dep := e.deployment(t, "plain", "vllm", metricsSnapshot("vllm", metrics, false), e.nodes...)
		var out []string
		for _, inst := range e.reconcile(t, dep.ID, 2) {
			// The stored contract is where the declaration itself is written.
			var document map[string]any
			if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &document); err != nil {
				t.Fatal(err)
			}
			if launch, ok := document["launch"].(map[string]any); ok {
				delete(launch, "api_contract")
			}
			encoded, _ := json.Marshal(document)
			text := string(encoded)
			for _, volatile := range []string{inst.UUID, inst.DistGroup} {
				if volatile != "" {
					text = strings.ReplaceAll(text, volatile, "<volatile>")
				}
			}
			// Nodes are named after the test; the rest is the launch.
			for _, node := range e.nodes {
				text = strings.ReplaceAll(text, node.UUID, "<node>")
			}
			out = append(out, fmt.Sprintf("rank %d ports %s: %s", inst.NodeRank, inst.Ports, text))
		}
		slices.Sort(out)
		return strings.Join(out, "\n")
	}
	var baseline string
	t.Run("no api.metrics", func(t *testing.T) { baseline = desired(t, nil) })
	for name, metrics := range map[string]map[string]any{
		"port api": {"port": "api"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := desired(t, metrics); got != baseline {
				t.Errorf("changed the desired state:\n got  %s\n want %s", got, baseline)
			}
		})
	}
}

// Two instances that declare the port and one that does not, all on one node,
// are given disjoint ports: every instance's API port and metrics port is
// held by it alone, and a reconcile pass that changes nothing leaves them
// where they are.
func TestColocatedInstancesNeverShareAMetricsPort(t *testing.T) {
	e := newMetricsEnv(t, 1)
	node := e.nodes[0]
	separate := map[string]any{"port": "separate"}
	var deps []*store.Deployment
	for index, declared := range []bool{true, false, true} {
		var metrics map[string]any
		if declared {
			metrics = separate
		}
		engine := []string{"vllm", "sglang", "callable"}[index]
		deps = append(deps, e.deployment(t, fmt.Sprintf("colocated-%d", index), engine, metricsSnapshot(engine, metrics, declared), node))
	}
	held := map[int]string{}
	for index, dep := range deps {
		instances := e.reconcile(t, dep.ID, 1)
		if len(instances) != 1 {
			t.Fatalf("deployment %d instances = %d", index, len(instances))
		}
		spec := desiredOf(t, instances[0])
		want := 1
		if index != 1 {
			want = 2
		}
		if len(spec.Ports) != want || spec.Ports[0] != spec.APIPort || (want == 2 && (spec.MetricsPort != spec.Ports[1] || spec.MetricsPort == spec.APIPort)) || (want == 1 && spec.MetricsPort != 0) {
			t.Fatalf("deployment %d: api %d metrics %d ports %v", index, spec.APIPort, spec.MetricsPort, spec.Ports)
		}
		for _, port := range spec.Ports {
			if owner, taken := held[port]; taken {
				t.Fatalf("port %d is held by both %s and deployment %d", port, owner, index)
			}
			held[port] = fmt.Sprintf("deployment %d", index)
		}
		if instances[0].Ports != jsonInts(spec.Ports) {
			t.Fatalf("deployment %d instance ports %s, desired ports %v", index, instances[0].Ports, spec.Ports)
		}
	}
	if len(held) != 5 {
		t.Fatalf("held ports = %v, want five distinct (2+1+2)", held)
	}
	before := map[uint]string{}
	for _, dep := range deps {
		for _, inst := range e.reconcile(t, dep.ID, 3) {
			before[inst.ID] = inst.DesiredStateJSON + inst.Ports
		}
	}
	if len(before) != 3 {
		t.Fatalf("steady passes changed the instance set: %d rows", len(before))
	}
}

// A server restart is a new controller over the same database: it launches
// nothing again and the instance's desired state, the one it pushes to an
// agent that reconnects (DesiredStateForNode), still names the reserved
// metrics port, on the wire as on disk. A replacement the controller builds
// after the instance fails reserves a metrics port of its own, again distinct
// from every instance still on the node.
func TestMetricsPortSurvivesServerRestartReconnectAndReplacement(t *testing.T) {
	e := newMetricsEnv(t, 1)
	node := e.nodes[0]
	separate := map[string]any{"port": "separate"}
	live := e.deployment(t, "live", "vllm", metricsSnapshot("vllm", separate, true), node)
	other := e.deployment(t, "other", "vllm", metricsSnapshot("vllm", separate, true), node)
	liveInst := e.reconcile(t, live.ID, 1)[0]
	otherInst := e.reconcile(t, other.ID, 1)[0]
	liveSpec := desiredOf(t, liveInst)
	if liveSpec.MetricsPort == 0 {
		t.Fatalf("no metrics port reserved: %s", liveInst.DesiredStateJSON)
	}
	if err := e.st.SetInstanceState(liveInst.UUID, proto.InstRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Server restart: a fresh controller, the same rows.
	restarted := New(e.st, e.hub, nil)
	restarted.SetRetryPolicy(0, 3)
	for pass := 0; pass < 2; pass++ {
		if err := restarted.reconcileDeployment(context.Background(), live.ID); err != nil {
			t.Fatal(err)
		}
	}
	after, err := e.st.InstancesForDeployment(live.ID)
	if err != nil || len(after) != 1 || after[0].UUID != liveInst.UUID || after[0].DesiredStateJSON != liveInst.DesiredStateJSON || after[0].Ports != liveInst.Ports {
		t.Fatalf("the restarted controller changed the instance: %+v err=%v", after, err)
	}

	// Agent reconnect: the full desired state the node receives.
	desired, err := e.st.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	var carried *proto.InstanceDesiredState
	for index := range desired.Instances {
		if desired.Instances[index].UUID == liveInst.UUID {
			carried = &desired.Instances[index]
		}
	}
	if carried == nil || carried.MetricsPort != liveSpec.MetricsPort || carried.APIPort != liveSpec.APIPort || !slices.Equal(carried.Ports, liveSpec.Ports) {
		t.Fatalf("reconnect desired state = %+v, want api %d metrics %d ports %v", carried, liveSpec.APIPort, liveSpec.MetricsPort, liveSpec.Ports)
	}
	wire, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	var decoded proto.DesiredState
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"metrics_port":`+strconv.Itoa(liveSpec.MetricsPort)) || !reflect.DeepEqual(decoded.Instances, desired.Instances) {
		t.Fatalf("the wire form lost the metrics port: %s", wire)
	}

	// The instance fails; the controller builds its replacement.
	if err := e.st.SetInstanceFailed(liveInst.UUID, proto.RuntimeFailure{Code: proto.ErrorContainerExited}, "engine exited"); err != nil {
		t.Fatal(err)
	}
	rows := e.reconcile(t, live.ID, 3)
	var replacement *store.Instance
	for index := range rows {
		if rows[index].UUID != liveInst.UUID && rows[index].State != proto.InstFailed {
			replacement = &rows[index]
		}
	}
	if replacement == nil {
		t.Fatalf("no replacement was built: %+v", rows)
	}
	replacementSpec := desiredOf(t, *replacement)
	otherSpec := desiredOf(t, otherInst)
	if replacementSpec.MetricsPort == 0 || len(replacementSpec.Ports) != 2 || replacementSpec.Ports[1] != replacementSpec.MetricsPort {
		t.Fatalf("replacement desired state = %s", replacement.DesiredStateJSON)
	}
	for _, port := range replacementSpec.Ports {
		if slices.Contains(otherSpec.Ports, port) {
			t.Fatalf("replacement ports %v overlap the other instance's %v", replacementSpec.Ports, otherSpec.Ports)
		}
	}
}

// A multi-node unit scrapes only its serving rank: the leader reserves the API
// port and the metrics port, a headless rank reserves nothing and is launched
// with ${GPUWORKS_METRICS_PORT} = 0 - as it launches with the API port 0 - and
// states no metrics port in its desired state.
func TestMultiNodeUnitReservesTheMetricsPortOnTheLeaderOnly(t *testing.T) {
	for _, engine := range []string{"vllm", "sglang"} {
		t.Run(engine, func(t *testing.T) {
			e := newMetricsEnv(t, 2)
			snapshot := metricsSnapshot(engine, map[string]any{"port": "separate"}, true)
			dep := e.deployment(t, "ranked", engine, snapshot, e.nodes...)
			instances := e.reconcile(t, dep.ID, 2)
			if len(instances) != 2 {
				t.Fatalf("instances = %d, want two ranks", len(instances))
			}
			var leader, follower *store.Instance
			for index := range instances {
				if instances[index].Headless {
					follower = &instances[index]
				} else {
					leader = &instances[index]
				}
			}
			if leader == nil || follower == nil {
				t.Fatalf("ranks = %+v", instances)
			}
			leaderSpec, followerSpec := desiredOf(t, *leader), desiredOf(t, *follower)
			if leaderSpec.MetricsPort != leaderSpec.APIPort+1 || !slices.Equal(leaderSpec.Ports, []int{leaderSpec.APIPort, leaderSpec.MetricsPort}) {
				t.Fatalf("leader api %d metrics %d ports %v", leaderSpec.APIPort, leaderSpec.MetricsPort, leaderSpec.Ports)
			}
			if got := instanceFlagValues(launchTokens(leaderSpec), "--metrics-listen"); !slices.Equal(got, []string{strconv.Itoa(leaderSpec.MetricsPort)}) {
				t.Fatalf("leader --metrics-listen=%v argv=%v", got, launchTokens(leaderSpec))
			}
			if _, present := rawDesired(t, *follower)["metrics_port"]; present || followerSpec.MetricsPort != 0 || len(followerSpec.Ports) != 0 || follower.Ports != "[]" || follower.APIPort != 0 {
				t.Fatalf("follower holds a port: %+v", follower)
			}
			if got := instanceFlagValues(launchTokens(followerSpec), "--metrics-listen"); !slices.Equal(got, []string{"0"}) {
				t.Fatalf("follower --metrics-listen=%v, want 0; argv=%v", got, launchTokens(followerSpec))
			}
			if !followerSpec.Headless {
				t.Fatalf("the follower must still launch as a headless rank: %+v", followerSpec)
			}
			// Both ranks reached their nodes: the headless one launches too.
			for _, node := range e.nodes {
				if len(e.hub.updates[node.UUID]) == 0 {
					t.Fatalf("node %s was never pushed its rank", node.UUID)
				}
			}
		})
	}
}

// Every PD leader reserves a metrics port of its own: the prefill instance and
// the decode unit's leader are scraped on different ports, held by nobody else.
func TestEveryPDLeaderHasItsOwnMetricsPort(t *testing.T) {
	f := newPDFixture(t, "pd_metrics_every_leader", twoPrefillOneDecode)
	snapshot := `{"extra_args":["--metrics-listen","${GPUWORKS_METRICS_PORT}"],"api":{"metrics":{"port":"separate"}}}`
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.dep.ID).Update("template_snapshot", snapshot).Error; err != nil {
		t.Fatal(err)
	}
	insts := f.reconcile(t)
	leaders := 0
	seen := map[int]string{}
	for _, inst := range insts {
		spec := desiredOf(t, inst)
		if inst.Headless {
			if spec.MetricsPort != 0 {
				t.Fatalf("PD follower %s has metrics port %d", inst.UUID, spec.MetricsPort)
			}
			continue
		}
		leaders++
		if spec.MetricsPort <= 0 || spec.MetricsPort == spec.APIPort {
			t.Fatalf("PD %s leader api %d metrics %d", inst.Role, spec.APIPort, spec.MetricsPort)
		}
		for _, port := range parseInts(inst.Ports) {
			key := inst.Role + "/" + inst.UUID
			if owner, taken := seen[port*1000+int(inst.NodeID)]; taken {
				t.Fatalf("port %d on node %d is held by %s and %s", port, inst.NodeID, owner, key)
			}
			seen[port*1000+int(inst.NodeID)] = key
		}
		if got := argAfter(spec.Argv, "--metrics-listen"); got != strconv.Itoa(spec.MetricsPort) {
			t.Fatalf("PD %s leader --metrics-listen=%q, want %d", inst.Role, got, spec.MetricsPort)
		}
	}
	if leaders != 3 {
		t.Fatalf("PD leaders = %d, want 3 (two prefill + one decode)", leaders)
	}
}

// An undeclared PD deployment keeps its ports as they were: no metrics port in
// any desired state.
func TestPDWithoutADeclarationReservesNoMetricsPort(t *testing.T) {
	f := newPDFixture(t, "pd_metrics_undeclared", prefillOneDecodeAcrossNodes)
	insts := f.reconcile(t)
	if len(insts) == 0 {
		t.Fatal("no PD instances")
	}
	for _, inst := range insts {
		if _, present := rawDesired(t, inst)["metrics_port"]; present {
			t.Fatalf("PD %s carries metrics_port: %s", inst.Role, inst.DesiredStateJSON)
		}
		spec := desiredOf(t, inst)
		if spec.MetricsPort != 0 || (!inst.Headless && !slices.Contains(spec.Ports, spec.APIPort)) {
			t.Fatalf("PD %s api %d metrics %d ports %v", inst.Role, spec.APIPort, spec.MetricsPort, spec.Ports)
		}
	}
}

// The node has no second port to give: the declaring instance is not launched
// with the API port alone (its engine would then be handed a metrics port that
// nothing reserved), the refusal is the engine-ports-exhausted reason, and
// nothing is pushed. The same node and range does take an instance that
// declares nothing.
func TestMetricsPortAllocationFailureRefusesTheLaunchInsteadOfFallingBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   int
		end     int
		blocked []int
	}{
		{"range of one port", 30000, 30000, nil},
		{"second port taken by a host listener", 30000, 30001, []int{30001}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newMetricsEnv(t, 1)
			node := e.nodes[0]
			e.ctrl.SetEnginePorts(tc.start, tc.end)
			if len(tc.blocked) > 0 {
				listeners, _ := json.Marshal(tc.blocked)
				if err := e.st.DB().Model(&store.Node{}).Where("id = ?", node.ID).
					Update("network_inventory", `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":`+string(listeners)+`}`).Error; err != nil {
					t.Fatal(err)
				}
			}
			declaring := e.deployment(t, "needs-two", "vllm", metricsSnapshot("vllm", map[string]any{"port": "separate"}, true), node)
			if instances := e.reconcile(t, declaring.ID, 2); len(instances) != 0 {
				t.Fatalf("a launch without a metrics port went ahead: %+v", instances)
			}
			if updates := e.hub.updates[node.UUID]; len(updates) != 0 {
				t.Fatalf("a refused launch pushed state: %+v", updates)
			}
			event, found, err := e.st.LatestScheduleEvent(declaring.ID)
			if err != nil || !found {
				t.Fatalf("no schedule event: found=%v err=%v", found, err)
			}
			for _, want := range []string{`"code":"engine_ports_exhausted"`, `"start":` + strconv.Itoa(tc.start), `"end":` + strconv.Itoa(tc.end)} {
				if !strings.Contains(event.Data, want) {
					t.Fatalf("schedule event %s lacks %s", event.Data, want)
				}
			}

			plain := e.deployment(t, "needs-one", "vllm", metricsSnapshot("vllm", nil, false), node)
			instances := e.reconcile(t, plain.ID, 2)
			if len(instances) != 1 || desiredOf(t, instances[0]).APIPort != tc.start {
				t.Fatalf("the same node should take the undeclared deployment on %d: %+v", tc.start, instances)
			}
		})
	}
}

// A multi-node unit whose leader cannot get both ports waits whole: no rank is
// launched, so no headless rank starts against a leader that never will.
func TestMultiNodeUnitWithoutTwoLeaderPortsLaunchesNoRank(t *testing.T) {
	e := newMetricsEnv(t, 2)
	e.ctrl.SetEnginePorts(30000, 30000)
	dep := e.deployment(t, "ranked-full", "vllm", metricsSnapshot("vllm", map[string]any{"port": "separate"}, true), e.nodes...)
	if instances := e.reconcile(t, dep.ID, 2); len(instances) != 0 {
		t.Fatalf("a unit whose leader has no metrics port launched ranks: %+v", instances)
	}
	for _, node := range e.nodes {
		if updates := e.hub.updates[node.UUID]; len(updates) != 0 {
			t.Fatalf("node %s was pushed state: %+v", node.UUID, updates)
		}
	}
}
