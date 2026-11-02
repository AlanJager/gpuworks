package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Benchmark target selection must judge sleep by planning.InstanceAsleep, the
// predicate the gateway and the deployment row use, not by the level the
// engine reports. A sleep order takes effect at once, before the engine
// reports the new level, and the gateway refuses the replica for that whole
// window. Selecting on the reported level alone therefore picked a replica
// the gateway was already refusing: the benchmark reached it directly, so
// instead of a clean refusal the operator got a 503 from an engine on its way
// to sleep, partway through a run.
//
// The benchmark menu item stays enabled on a partly asleep deployment (the row
// counts as running while any replica serves), so this is the state an
// operator is most likely to click the button in.
func TestBenchmarkTargetSkipsAReplicaOrderedAsleepBeforeTheEngineReports(t *testing.T) {
	a := newBenchmarkAdmission(t, "benchmark_sleep_intent")
	st := a.store
	node, err := st.RegisterNode("intent-node", store.HashToken("token-intent"), "intent-node")
	if err != nil {
		t.Fatal(err)
	}
	model := benchmarkChatModel(t, st, "intent-model")
	dep, err := st.CreateDeployment(&store.Deployment{Name: "intent", ModelID: model.ID, ServedName: "intent-served", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	// An instance only carries a sleep intent once it holds a desired state, so
	// seed one, then order the sleep and leave the reported level at 0: the
	// drain window, intent 1 / level 0.
	//
	// The draining replica is created FIRST on purpose. Selection walks the
	// instances in id order, so if the sleep judgement were ignored the
	// draining replica would be the one chosen; creating it first makes that
	// impossible to reach by luck.
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "intent-draining", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		APIPort: 31001, GPUIndexes: `[1]`,
		DesiredStateJSON: `{"uuid":"intent-draining","desired_state":"running","sleepable":true,"gpu_indexes":[1]}`,
	}); err != nil {
		t.Fatal(err)
	}
	awake, err := st.CreateInstance(&store.Instance{
		UUID: "intent-awake", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		APIPort: 31000, GPUIndexes: `[0]`,
		DesiredStateJSON: `{"uuid":"intent-awake","desired_state":"running","sleepable":true,"gpu_indexes":[0]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"intent-draining"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	draining, err := st.InstanceByUUID("intent-draining")
	if err != nil {
		t.Fatal(err)
	}
	if draining.SleepIntent() == 0 || draining.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (>0, 0)", draining.SleepIntent(), draining.SleepLevel)
	}

	handlers := a.handlers
	// Naming the draining instance must be refused, not accepted: the gateway
	// answers 503 for it, so a benchmark pointed at it measures nothing.
	target, refusal := handlers.resolveManagedBenchmarkTarget(
		a.request(t), dep.ID, "intent-draining", "auto")
	if refusal == nil {
		t.Fatalf("targeting a replica under a sleep order resolved to %+v, want a refusal", target)
	}
	if refusal.code != ErrorBenchmarkLeaderNotRunning || refusal.status != http.StatusConflict {
		t.Fatalf("refusal = %d %q, want 409 %q", refusal.status, refusal.code, ErrorBenchmarkLeaderNotRunning)
	}

	// Without naming one, the awake replica must be chosen, never the draining
	// one. This asks for a node origin, which is the path the UI uses when it
	// measures from the target's own node: both replicas sit on that node, so
	// only the sleep judgement decides which one is picked.
	target, refusal = handlers.resolveManagedBenchmarkTarget(a.request(t), dep.ID, "", node.UUID)
	if refusal != nil {
		t.Fatalf("resolving the deployment = %+v", refusal)
	}
	if target.instance == nil || target.instance.UUID != awake.UUID {
		t.Fatalf("leader = %+v, want the awake replica %q: picking the draining one benchmarks a replica the gateway refuses",
			target.instance, awake.UUID)
	}
	// The draining replica's card is not part of what a deployment-wide
	// benchmark would measure.
	if target.numGPUs != 1 {
		t.Fatalf("numGPUs = %d, want 1: only the awake replica's card is measured", target.numGPUs)
	}
}

// The same judgement for the snapshot a run records: the hardware a benchmark
// reports must describe the replicas that answered it. A replica ordered to
// sleep has already been refused by the gateway and holds no card it would
// answer from, so counting its GPU would attribute the run to hardware that
// never carried a token.
func TestBenchmarkSnapshotLeavesOutAReplicaOrderedAsleep(t *testing.T) {
	st, err := store.Open("file:benchmark_snapshot_intent?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("snap-intent-node", store.HashToken("token-snap-intent"), "snap-intent-node")
	if err != nil {
		t.Fatal(err)
	}
	for _, gpu := range []store.GPU{
		{NodeID: node.ID, Index: 0, UUID: "GPU-intent-0", Name: "A100", ArchID: "sm80", Vendor: "nvidia"},
		{NodeID: node.ID, Index: 1, UUID: "GPU-intent-1", Name: "A100", ArchID: "sm80", Vendor: "nvidia"},
	} {
		if err := st.DB().Create(&gpu).Error; err != nil {
			t.Fatal(err)
		}
	}
	model := benchmarkChatModel(t, st, "snap-intent-model")
	dep, err := st.CreateDeployment(&store.Deployment{Name: "snap-intent", ModelID: model.ID, ServedName: "snap-intent-served", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	leader, err := st.CreateInstance(&store.Instance{
		UUID: "snap-awake", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		APIPort: 31000, GPUIndexes: `[0]`,
		DesiredStateJSON: `{"uuid":"snap-awake","desired_state":"running","sleepable":true,"gpu_indexes":[0]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "snap-draining", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		APIPort: 31001, GPUIndexes: `[1]`,
		DesiredStateJSON: `{"uuid":"snap-draining","desired_state":"running","sleepable":true,"gpu_indexes":[1]}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"snap-draining"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	draining, err := st.InstanceByUUID("snap-draining")
	if err != nil {
		t.Fatal(err)
	}
	if draining.SleepIntent() == 0 || draining.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (>0, 0)", draining.SleepIntent(), draining.SleepLevel)
	}

	handlers := &apiHandlers{d: Deps{Store: st}}
	raw, err := handlers.benchmarkTargetSnapshot("instance", "instance", "http://127.0.0.1:31000",
		dep.ServedName, node.UUID, 1, dep, leader, insts)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot benchmarkTargetSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, gpu := range snapshot.GPUs {
		if gpu.Index == 1 {
			t.Fatalf("snapshot recorded GPU 1 of a replica ordered to sleep: %+v", snapshot.GPUs)
		}
	}
	if len(snapshot.GPUs) != 1 || snapshot.GPUs[0].Index != 0 {
		t.Fatalf("snapshot GPUs = %+v, want only the awake replica's GPU 0", snapshot.GPUs)
	}
}

// request is an authenticated create request: the deployment-wide target
// resolution reads the caller's claims, so it needs one.
func (a *benchmarkAdmission) request(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+a.token)
	return req
}
