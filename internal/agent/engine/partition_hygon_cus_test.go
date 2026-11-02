package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestHygonAllocatesCUsFromSelectedPhysicalCard(t *testing.T) {
	for _, tc := range []struct {
		name        string
		memoryMiB   int64
		physicalCUs int
		wantCUs     int
	}{
		{"Z100", 32752, 60, 2},
		{"K100AI", 65520, 128, 3},
		{"K100_AI", 65520, 128, 3},
		{"K100 AI", 65520, 128, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, h, fake := hygonTestEngine(t)
			p := e.prober.(*hygonTestProber)
			p.GPUs[0].ArchID = tc.name
			p.GPUs[0].MemoryTotalBytes = tc.memoryMiB << 20
			p.computeUnits = tc.physicalCUs
			ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, proto.InstanceDesiredState{
				UUID: "physical-cus", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20,
			})
			if !ok {
				t.Fatal(reason)
			}
			if len(fake.createdCUs) != 1 || fake.createdCUs[0] != tc.wantCUs {
				t.Fatalf("allocated CUs = %v, want %d", fake.createdCUs, tc.wantCUs)
			}
			if err := h.cleanupInstance(e, "physical-cus"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHygonRejectsUnavailablePhysicalCUsBeforeCreate(t *testing.T) {
	for _, failure := range []string{"missing capability", "query error", "zero count", "negative count", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			e, h, fake := hygonTestEngine(t)
			p := e.prober.(*hygonTestProber)
			ctx := context.Background()
			switch failure {
			case "missing capability":
				e.prober = p.FixedProber
			case "query error":
				p.computeErr = errors.New("driver unavailable")
			case "zero count":
				p.computeUnits = 0
			case "negative count":
				p.computeUnits = -1
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			ok, reason := h.apply(e, ctx, &agentruntime.WorkloadSpec{}, proto.InstanceDesiredState{
				UUID: "failed-cus", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20,
			})
			if ok || !strings.Contains(reason, "compute-unit") && !strings.Contains(reason, "compute units") {
				t.Fatalf("unverified count accepted: %t/%s", ok, reason)
			}
			if len(fake.createdCUs) != 0 {
				t.Fatalf("unverified count created vdevs: %v", fake.createdCUs)
			}
		})
	}
}
