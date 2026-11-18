package scheduler_test

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

// DP copies consume distinct cards but do not divide a replica's weights.
// Combine shape with exact/short memory and pin width; a spare card outside
// the selection must not repair an undersized selection.
func TestParallelShapeByMemoryAndPinWidth(t *testing.T) {
	for _, shape := range []struct {
		name  string
		spec  parallelism.Spec
		cards int
		need  int64
	}{
		{"single", parallelism.Spec{TP: 1}, 1, 18 * gib},
		{"tensor", parallelism.Spec{TP: 2}, 2, 10 * gib},
		{"pipeline", parallelism.Spec{TP: 1, PP: 2}, 2, 10 * gib},
		{"data copies", parallelism.Spec{TP: 1, DP: 2}, 2, 18 * gib},
		{"tensor and data", parallelism.Spec{TP: 2, DP: 2}, 4, 10 * gib},
		{"two-node tensor", parallelism.Spec{TP: 2, NNodes: 2}, 1, 10 * gib},
		{"two-node tensor pipeline data", parallelism.Spec{TP: 2, PP: 2, DP: 2, NNodes: 2}, 4, 6 * gib},
	} {
		for _, short := range []bool{false, true} {
			for _, pin := range []string{"automatic", "exact", "too-narrow"} {
				t.Run(fmt.Sprintf("%s/short-%v/%s", shape.name, short, pin), func(t *testing.T) {
					in := scheduleInput{request: scheduler.Request{WeightBytes: 16 * gib, ReserveBytes: 2 * gib, GPUMemUtil: 0.5, HeadCount: 8, Parallel: shape.spec}, node: scheduler.NodeSnapshot{UUID: "parallel", Vendor: "nvidia", DtypeSupported: true}}
					for i := 0; i < shape.cards+1; i++ {
						g := card(2*i + 1)
						g.MemoryTotal = 2 * shape.need
						if short {
							g.MemoryTotal -= 2
						}
						in.node.GPUs = append(in.node.GPUs, g)
						if pin != "automatic" && i < shape.cards {
							in.request.AllowedGPUIndexes = append(in.request.AllowedGPUIndexes, g.Index)
						}
					}
					if pin == "too-narrow" {
						// An empty list means automatic; use a nonexistent index when
						// the one-card selection is shortened to zero real cards.
						in.request.AllowedGPUIndexes = in.request.AllowedGPUIndexes[:shape.cards-1]
						in.request.AllowedGPUIndexes = append(in.request.AllowedGPUIndexes, 99)
					}
					want := scheduleExpectation{ok: !short && pin != "too-narrow", tp: shape.cards}
					result := runScheduleCase(t, scheduleCase{input: in, want: want})
					if want.ok {
						if result.Plan.PerGPUBytes != shape.need {
							t.Fatalf("per-card bytes=%d, want %d; DP must not dilute weights", result.Plan.PerGPUBytes, shape.need)
						}
					}
				})
			}
		}
	}
}
