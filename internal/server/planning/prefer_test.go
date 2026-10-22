package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template whose resolver accepts four or eight GPUs and prefers the larger
// takes eight on a free eight-GPU node, four when only four are free or the
// user named four, and the fewest when it states no preference.
func TestEvaluateFollowsTheResolversGPUCountPreference(t *testing.T) {
	st, err := store.Open("file:planning_prefer_gpu_count?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("prefer-node", store.HashToken("token"), "prefer-node")
	if err != nil {
		t.Fatal(err)
	}
	node.DiskTotalKB = 1 << 30
	for index := range 8 {
		node.GPUs = append(node.GPUs, store.GPU{Index: index, MemoryTotal: 80 << 30, Vendor: "ascend", ArchID: "910B4"})
	}
	const resolver = `def resolve(gpu):
    if gpu.count != 4 and gpu.count != 8:
        return reject("requires four or eight GPUs")
    return accept(gpu_memory_mib_per_device=4096, variables={"GPU_COUNT": gpu.count, "TP_SIZE": gpu.count}, prefer=gpu.count)`
	indifferent := `def resolve(gpu):
    if gpu.count != 4 and gpu.count != 8:
        return reject("requires four or eight GPUs")
    return accept(gpu_memory_mib_per_device=4096, variables={"GPU_COUNT": gpu.count, "TP_SIZE": gpu.count})`
	model := &store.Model{Name: "prefer", WeightBytes: 8 << 30}
	input := func(source string) Input {
		return Input{Model: model, Node: node, Engine: "sglang", PlacementResolver: source, Quick: launchConfig(t, "sglang")}
	}

	out := input(resolver).Evaluate(st)
	if !out.Result.OK || out.Result.Plan.TP != 8 || out.PlacementError != nil {
		t.Fatalf("free eight-GPU node: %+v err=%v", out.Result, out.PlacementError)
	}
	if out.Placement.Variables["TP_SIZE"] != int64(8) || out.Placement.Prefer != 8 {
		t.Fatalf("the final resolve must describe the chosen set: %+v", out.Placement)
	}

	out = input(indifferent).Evaluate(st)
	if !out.Result.OK || out.Result.Plan.TP != 4 || out.Result.Plan.Preference != 0 {
		t.Fatalf("no preference must keep the fewest accepted GPUs: %+v", out.Result)
	}

	pinned := input(resolver)
	pinned.GPUIndexes = []int{0, 1, 2, 3}
	out = pinned.Evaluate(st)
	if !out.Result.OK || out.Result.Plan.TP != 4 {
		t.Fatalf("named GPUs pin the count: %+v err=%v", out.Result, out.PlacementError)
	}

	node.GPUs = node.GPUs[:4]
	out = input(resolver).Evaluate(st)
	if !out.Result.OK || out.Result.Plan.TP != 4 {
		t.Fatalf("four GPUs: %+v", out.Result)
	}
	for _, reason := range out.Result.Plan.Reasons {
		if reason.Code == scheduler.ReasonPlacementPreference && reason.Params["count"] != 4 {
			t.Fatalf("preference reason = %+v", reason)
		}
	}
}
