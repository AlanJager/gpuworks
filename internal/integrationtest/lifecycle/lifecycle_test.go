package lifecycle

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestRegisterAndHeartbeatThroughRealAppWiring(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("gpu-a", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.AddNode("gpu-u", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("udocker"))
	env.WaitNodeOnline(t, "gpu-a")
	env.WaitNodeOnline(t, "gpu-u")
	// WaitNodeOnline intentionally only gates the session state.  Hardware and
	// runtime metadata arrive in the following status frame, so read the rows
	// only after that asynchronous update has completed as well.
	var nodes []store.Node
	env.Eventually(t, 10*time.Second, func() bool {
		got, err := env.Store().ListNodes()
		if err != nil || len(got) != 2 {
			return false
		}
		for _, node := range got {
			caps := partitioncap.ForNode(&node)
			if node.GPUCount != 1 || node.GPUVendor != "nvidia" || node.Hostname == "" || node.CPUCores <= 0 || len(caps) != 1 || caps[0].Backend != accelerator.BackendNVIDIALogical || caps[0].Status != partitioncap.StatusReady {
				return false
			}
		}
		nodes = got
		return true
	})
	for _, node := range nodes {
		caps := partitioncap.ForNode(&node)
		if len(caps) != 1 || caps[0].Status != partitioncap.StatusReady {
			t.Fatalf("wire-reported partition capabilities for runtime %q=%+v, want nvidia-dgpu ready", node.RuntimeDriver, caps)
		}
	}
}
