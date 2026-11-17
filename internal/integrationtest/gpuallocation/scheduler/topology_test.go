package scheduler_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// The strongest interconnect only wins among admissible groups. If its cards
// are occupied or too small, placement must retry the remaining topology.
func TestTopologyByCapacityAndPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*scheduleInput)
		want   scheduleExpectation
	}{
		{"NVLink fits", func(*scheduleInput) {}, scheduleExpectation{ok: true, indexes: []int{2, 3}, tp: 2}},
		{"NVLink occupied", func(in *scheduleInput) { in.node.GPUs[2].AllocatedBytes = gib }, scheduleExpectation{ok: true, indexes: []int{0, 1}, tp: 2}},
		{"NVLink too small", func(in *scheduleInput) { in.node.GPUs[2].MemoryTotal = 16 * gib }, scheduleExpectation{ok: true, indexes: []int{0, 1}, tp: 2}},
		{"pin overrides topology", func(in *scheduleInput) { in.request.AllowedGPUIndexes = []int{0, 1} }, scheduleExpectation{ok: true, indexes: []int{0, 1}, tp: 2}},
		{"pin cannot meet topology count", func(in *scheduleInput) { in.request.AllowedGPUIndexes = []int{2} }, scheduleExpectation{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := scheduleInput{request: scheduler.Request{WeightBytes: 80 * gib, ReserveBytes: 2 * gib, GPUMemUtil: 0.9, AllowedTPs: []int{2}, HeadCount: 8},
				node: scheduler.NodeSnapshot{UUID: "topology", Vendor: "nvidia", DtypeSupported: true, GPUs: []scheduler.GPU{card(0), card(1), card(2), card(3)}, Topology: &topoparse.Topology{NVLinkCliques: [][]int{{2, 3}}}}}
			// Only the first pair shares a NUMA domain, making the fallback unique.
			in.node.GPUs[2].NUMANode = 1
			in.node.GPUs[3].NUMANode = 2
			tc.change(&in)
			runScheduleCase(t, scheduleCase{input: in, want: tc.want})
		})
	}
}
