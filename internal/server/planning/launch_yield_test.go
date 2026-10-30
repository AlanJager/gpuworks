package planning

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A launch that does not fit fits once the awake instances of deployments
// that share its cards would sleep: launchYield names them, and only for a
// newcomer that shares with them.
func TestALaunchWaitsForTheDeploymentsThatShareItsCards(t *testing.T) {
	st, err := store.Open("file:planning_launch_yield_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("yield-node", store.HashToken("token"), "yield-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
	mk := func(name, yield string) *store.Deployment {
		t.Helper()
		dep := &store.Deployment{Name: name, ModelID: 1, NodeID: node.ID, ServedName: name, Engine: "vllm", Sleepable: true, State: "active"}
		p, _ := servicelevel.Parse(yield, servicelevel.Overrides{})
		dep.SetPolicy(p)
		created, err := st.CreateDeployment(dep)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	batch := mk("batch", servicelevel.Anytime)
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-1", DeploymentID: batch.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("batch-1", 0)}); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{WeightBytes: 8 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	plan := func(policy string) *LaunchYield {
		t.Helper()
		view := ledgerView{reserved: reservedAgainst(st, policy)}
		if scheduler.Schedule(req, nodeSnapshot(st, node, true, "", view)).OK {
			t.Fatal("the card is free with batch awake on it")
		}
		return launchYield(st, node, policy, req, true, "", false, view)
	}

	yield := plan(servicelevel.Idle)
	if yield == nil || len(yield.Holders) != 1 || yield.Holders[0].Instance != "batch-1" || yield.Holders[0].Deployment != "batch" || len(yield.GPUIndexes) != 1 {
		t.Fatalf("idle newcomer = %+v, want it to wait for batch on card 0", yield)
	}
	reason := YieldRefusal(node, servicelevel.Policy{Yield: servicelevel.Idle}, yield)
	if reason.Code != scheduler.ReasonWaitingForYield || jsonText(t, reason.Params) != `{"holders":[{"at_once":true,"deployment":"batch"}],"instances":["batch-1"],"node":"yield-node"}` {
		t.Fatalf("refusal = %s %s", reason.Code, jsonText(t, reason.Params))
	}
	// An equal waits for batch to go idle.
	if reason := YieldRefusal(node, servicelevel.Policy{Yield: servicelevel.Anytime}, plan(servicelevel.Anytime)); jsonText(t, reason.Params["holders"]) != `[{"at_once":false,"deployment":"batch","idle_sec":30}]` {
		t.Fatalf("anytime newcomer holders = %s", jsonText(t, reason.Params["holders"]))
	}
	// A never newcomer shares with no one, and waits for nobody.
	if yield := plan(servicelevel.Never); yield != nil {
		t.Fatalf("never newcomer = %+v", yield)
	}
	// A never holder gives way to no one.
	if err := st.SetDeploymentYieldPolicy(batch.ID, servicelevel.Policy{Yield: servicelevel.Never}); err != nil {
		t.Fatal(err)
	}
	if yield := plan(servicelevel.Idle); yield != nil {
		t.Fatalf("waiting for a never holder = %+v", yield)
	}
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A holder whose sleep is ordered and has not landed is counted asleep: the
// launch waits for nothing more than that sleep.
func TestALaunchWaitsForASleepAlreadyOrdered(t *testing.T) {
	st, err := store.Open("file:planning_launch_yield_ordered?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("ordered-node", store.HashToken("token"), "ordered-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
	dep := &store.Deployment{Name: "batch", ModelID: 1, NodeID: node.ID, ServedName: "batch", Engine: "vllm", Sleepable: true, State: "active"}
	p, _ := servicelevel.Parse(servicelevel.Anytime, servicelevel.Overrides{})
	dep.SetPolicy(p)
	batch, err := st.CreateDeployment(dep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-1", DeploymentID: batch.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("batch-1", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(batch.ID, []string{"batch-1"}, 1, proto.SleepModeAbort); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{WeightBytes: 8 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	view := ledgerView{reserved: reservedAgainst(st, servicelevel.Idle)}
	if scheduler.Schedule(req, nodeSnapshot(st, node, true, "", view)).OK {
		t.Fatal("a sleep that has not landed already freed the card")
	}
	yield := launchYield(st, node, servicelevel.Idle, req, true, "", false, view)
	if yield == nil || len(yield.Holders) != 0 {
		t.Fatalf("yield = %+v, want a wait with no holder to put to sleep", yield)
	}
	if reason := YieldRefusal(node, servicelevel.Policy{Yield: servicelevel.Idle}, yield); reason.Code != scheduler.ReasonWaitingForYield {
		t.Fatalf("refusal = %+v", reason)
	}
}
