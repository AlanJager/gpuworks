package scheduler_test

import (
	"reflect"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

const gib int64 = 1 << 30

type scheduleInput struct {
	request scheduler.Request
	node    scheduler.NodeSnapshot
}

type scheduleExpectation struct {
	ok      bool
	indexes []int
	tp      int
	code    string
}

type scheduleCase struct {
	input scheduleInput
	want  scheduleExpectation
}

func card(index int) scheduler.GPU {
	return scheduler.GPU{Index: index, MemoryTotal: 64 * gib, Schedulable: true,
		PartitionBackends: []string{accelerator.BackendNVIDIALogical}}
}

func runScheduleCase(t *testing.T, tc scheduleCase) {
	t.Helper()
	result := scheduler.Schedule(tc.input.request, tc.input.node)
	if result.OK != tc.want.ok || (result.Plan != nil) != tc.want.ok {
		t.Fatalf("want OK=%v, got %+v", tc.want.ok, result)
	}
	if !result.OK {
		if result.Message.Code == "" || (tc.want.code != "" && result.Message.Code != tc.want.code) {
			t.Fatalf("refusal=%+v, want code %q", result.Message, tc.want.code)
		}
		return
	}
	if tc.want.indexes != nil && !reflect.DeepEqual(result.Plan.GPUIndexes, tc.want.indexes) {
		t.Fatalf("indexes=%v, want %v", result.Plan.GPUIndexes, tc.want.indexes)
	}
	if tc.want.tp != 0 && result.Plan.TP != tc.want.tp {
		t.Fatalf("TP=%d, want %d", result.Plan.TP, tc.want.tp)
	}
	assertScheduleInvariants(t, tc.input, result)
}

func assertScheduleInvariants(t *testing.T, in scheduleInput, result scheduler.Result) {
	t.Helper()
	seen := map[int]bool{}
	for _, index := range result.Plan.GPUIndexes {
		if seen[index] || !slices.ContainsFunc(in.node.GPUs, func(g scheduler.GPU) bool { return g.Index == index }) {
			t.Fatalf("duplicate or unknown card in %+v", result.Plan)
		}
		seen[index] = true
		if len(in.request.AllowedGPUIndexes) > 0 && !slices.Contains(in.request.AllowedGPUIndexes, index) {
			t.Fatalf("plan escaped the pinned cards: %+v", result.Plan)
		}
	}
	if len(seen) != result.Plan.TP || result.Plan.PerGPUBytes <= 0 {
		t.Fatalf("invalid group or memory requirement: %+v", result.Plan)
	}
}

// Selection is a different matrix from per-card admission. These cases force
// the planner to choose between alternatives, including a legal fallback.
func TestSelectionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*scheduleInput)
		want   scheduleExpectation
	}{
		{"pinned sparse index", func(in *scheduleInput) { in.request.AllowedGPUIndexes = []int{7} }, scheduleExpectation{ok: true, indexes: []int{7}, tp: 1}},
		{"unknown pin", func(in *scheduleInput) { in.request.AllowedGPUIndexes = []int{99} }, scheduleExpectation{}},
		{"busy pin cannot escape", func(in *scheduleInput) { in.request.AllowedGPUIndexes = []int{7}; in.node.GPUs[0].AllocatedBytes = gib }, scheduleExpectation{}},
		{"automatic skips busy card", func(in *scheduleInput) { in.node.GPUs[0].AllocatedBytes = gib }, scheduleExpectation{ok: true, indexes: []int{19}, tp: 1}},
		{"healthy before degraded", func(in *scheduleInput) { in.node.GPUs[0].Degraded = true }, scheduleExpectation{ok: true, indexes: []int{19}, tp: 1}},
		{"degraded fallback", func(in *scheduleInput) { in.node.GPUs[0].Degraded = true; in.node.GPUs[1].AllocatedBytes = gib }, scheduleExpectation{ok: true, indexes: []int{7}, tp: 1}},
		{"weights need two cards", func(in *scheduleInput) { in.request.WeightBytes = 80 * gib }, scheduleExpectation{ok: true, indexes: []int{7, 19}, tp: 2}},
		{"head count forbids sharding", func(in *scheduleInput) { in.request.WeightBytes = 80 * gib; in.request.HeadCount = 7 }, scheduleExpectation{}},
		{"pipeline group", func(in *scheduleInput) { in.request.Parallel = parallelism.Spec{TP: 1, PP: 2} }, scheduleExpectation{ok: true, indexes: []int{7, 19}, tp: 2}},
		{"dtype unavailable", func(in *scheduleInput) { in.node.DtypeSupported = false }, scheduleExpectation{code: scheduler.ReasonDtypeUnsupported}},
		{"serving network unavailable", func(in *scheduleInput) { in.node.ServingUnavailable = true }, scheduleExpectation{code: scheduler.ReasonServingUnavailable}},
		{"model disk short", func(in *scheduleInput) { in.request.DiskNeedBytes = 2 * gib; in.node.DiskAvailBytes = gib }, scheduleExpectation{code: scheduler.ReasonInsufficientDisk}},
		{"runtime disk short", func(in *scheduleInput) {
			in.request.RuntimeDiskNeedBytes = 2 * gib
			in.node.RuntimeDiskAvailBytes = gib
		}, scheduleExpectation{code: scheduler.ReasonInsufficientRuntimeDisk}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := scheduleInput{request: scheduler.Request{WeightBytes: 8 * gib, ReserveBytes: 2 * gib, GPUMemUtil: 0.9, HeadCount: 8},
				node: scheduler.NodeSnapshot{UUID: "selection", Vendor: "nvidia", DtypeSupported: true, GPUs: []scheduler.GPU{card(7), card(19)}}}
			tc.change(&in)
			runScheduleCase(t, scheduleCase{input: in, want: tc.want})
		})
	}
}
