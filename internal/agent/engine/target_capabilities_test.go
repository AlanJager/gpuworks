package engine

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
	"testing"
)

func TestTargetDesiredLeavesOtherVendorLaunchPolicyToServer(t *testing.T) {
	for _, tc := range []struct {
		vendor string
		spec   proto.InstanceDesiredState
	}{
		{vendor: "apple", spec: proto.InstanceDesiredState{}},
		{vendor: "nvidia", spec: proto.InstanceDesiredState{NNodes: 2}},
		{vendor: "ascend", spec: proto.InstanceDesiredState{SMLimitPct: 50}},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			factsRead := false
			e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: tc.vendor}, WithHostRuntimeFacts(func() accelerator.HostRuntime {
				factsRead = true
				return accelerator.HostRuntime{}
			}))
			if err := e.validateTargetDesired(tc.spec); err != nil {
				t.Fatalf("agent changed existing %s launch admission: %v", tc.vendor, err)
			}
			if !factsRead {
				t.Fatalf("non-AMD %s guard did not evaluate host runtime facts", tc.vendor)
			}
		})
	}
}

func TestEngineDoesNotAllocateDefaultHostFactsReader(t *testing.T) {
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: "nvidia"})
	if e.hostRuntimeFacts != nil {
		t.Fatal("engine eagerly allocated a default host runtime facts reader")
	}
}

type preflightTrackingDriver struct {
	*headlessTestDriver
	statusCalls int
	runCalls    int
}
