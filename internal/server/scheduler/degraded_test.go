package scheduler

import (
	"reflect"
	"testing"
)

func degradedGPU(idx int, memGiB int64, numa int) GPU {
	g := gpu(idx, memGiB, true, numa)
	g.Degraded = true
	return g
}

func hasReason(p *Plan, code string) *Reason {
	for i := range p.Reasons {
		if p.Reasons[i].Code == code {
			return &p.Reasons[i]
		}
	}
	return nil
}

// A degraded GPU listed first must not be chosen while a fully healthy one can
// take the request.
func TestScheduleUsesHealthyGPUsBeforeDegradedOnes(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{degradedGPU(0, 48, 0), degradedGPU(1, 48, 0), gpu(2, 48, true, 0)},
	}
	res := Schedule(Request{WeightBytes: 16 * giB, HeadCount: 32, GPUMemUtil: 0.9}, node)
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{2}) {
		t.Fatalf("plan = %+v, %+v", res.Plan, res.Message)
	}
	if hasReason(res.Plan, ReasonDegradedGPUsUsed) != nil {
		t.Fatalf("a plan on healthy GPUs reports degraded use: %+v", res.Plan.Reasons)
	}
}

// When the healthy GPUs cannot take the request the degraded ones are used,
// and the plan names them.
func TestScheduleFallsBackToDegradedGPUs(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, false, 0), degradedGPU(1, 48, 0), gpu(2, 48, true, 0)},
	}
	// tp=2 is needed and only one healthy GPU is free.
	res := Schedule(Request{WeightBytes: 64 * giB, HeadCount: 40, GPUMemUtil: 0.9}, node)
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{1, 2}) {
		t.Fatalf("plan = %+v, %+v", res.Plan, res.Message)
	}
	reason := hasReason(res.Plan, ReasonDegradedGPUsUsed)
	if reason == nil || !reflect.DeepEqual(reason.Params["gpus"], []int{1}) || reason.Params["count"] != 1 || reason.Score != 0 {
		t.Fatalf("degraded reason = %+v", reason)
	}
}

// A manual selection of a degraded GPU is honoured.
func TestScheduleHonoursManualSelectionOfDegradedGPU(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), degradedGPU(1, 48, 0)},
	}
	res := Schedule(Request{WeightBytes: 16 * giB, HeadCount: 32, GPUMemUtil: 0.9, AllowedGPUIndexes: []int{1}}, node)
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{1}) || hasReason(res.Plan, ReasonDegradedGPUsUsed) == nil {
		t.Fatalf("plan = %+v, %+v", res.Plan, res.Message)
	}
}

// A node whose GPUs are all degraded still schedules.
func TestScheduleOnAllDegradedNode(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{degradedGPU(0, 48, 0)},
	}
	res := Schedule(Request{WeightBytes: 16 * giB, HeadCount: 32, GPUMemUtil: 0.9}, node)
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{0}) {
		t.Fatalf("plan = %+v, %+v", res.Plan, res.Message)
	}
}
