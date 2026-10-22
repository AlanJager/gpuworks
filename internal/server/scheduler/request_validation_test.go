package scheduler

import "testing"

// ADR 0020: a request that did not carry the effective utilization is an
// error, never a silent 0.9. Planning materializes the value before it gets
// here, so hitting this branch means a caller bypassed materialization.
func TestScheduleRejectsRequestWithoutGPUMemUtil(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1",
		GPUs: []GPU{{Index: 0, MemoryTotal: 80 << 30, Schedulable: true}},

		DtypeSupported: true,
	}
	for _, util := range []float64{0, -0.1, 1.5} {
		req := Request{WeightBytes: 1 << 30, GPUMemUtil: util}
		if err := req.Validate(); err == nil {
			t.Errorf("util=%v must fail validation", util)
		}
		res := Schedule(req, node)
		if res.OK {
			t.Errorf("util=%v must not schedule: %+v", util, res)
		}
		if res.Message.Code != ReasonInvalidRequest {
			t.Errorf("util=%v reason=%q, want %q", util, res.Message.Code, ReasonInvalidRequest)
		}
	}
	valid := Request{WeightBytes: 1 << 30, GPUMemUtil: 0.92}
	if err := valid.Validate(); err != nil {
		t.Fatalf("util=0.92 must validate: %v", err)
	}
	res := Schedule(valid, node)
	if !res.OK || res.Plan == nil {
		t.Fatalf("a valid request on an empty 80 GiB GPU must schedule: %+v", res)
	}
	if got := node.GPUs[0].CapacityFor(valid); got != int64(float64(node.GPUs[0].MemoryTotal)*0.92) {
		t.Fatalf("capacity uses the request's util verbatim, got %d", got)
	}
}
