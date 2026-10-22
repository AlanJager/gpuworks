package testenv

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A node is online as soon as its session registers; its GPUs arrive with the
// first status report. WaitNodeOnline must not hand a node back before that
// report, or a deployment created next is refused for want of a healthy GPU
// (DEPLOYMENT_NEVER_FITS / no_healthy_gpu on a loaded CI host). The probe gate
// withholds the report so the window is held open instead of raced.
func TestWaitNodeOnlineWaitsForTheFirstStatusReport(t *testing.T) {
	env := New(t)
	gate := make(chan struct{})
	node := env.AddNode("gated", WithGPUs(1, GPUSpec{Model: "A100", VRAMMB: 24576}), WithProbeGate(gate))
	env.Eventually(t, 30*time.Second, func() bool {
		row, _ := env.nodeState(node.Name)
		return row.State == store.NodeStateOnline
	})
	// Registered and online, but no report yet: not ready, and no GPU row.
	time.Sleep(300 * time.Millisecond)
	if ready, state := env.nodeReady(node.Name); ready {
		t.Fatalf("node reported ready before its first status report: %s", state)
	}
	withGPUs, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil || len(withGPUs.GPUs) != 0 {
		t.Fatalf("GPUs before the report: %+v err=%v", withGPUs, err)
	}
	close(gate)
	env.WaitNodeOnline(t, node.Name)
	withGPUs, err = env.Store().NodeWithGPUs(node.UUID)
	if err != nil || len(withGPUs.GPUs) != 1 {
		t.Fatalf("WaitNodeOnline returned before the GPU inventory landed: %+v err=%v", withGPUs, err)
	}
}
