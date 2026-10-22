//go:build cloudgpu && autodl

package l4

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// TestAutoDLNodeOnboarding validates the documented Pro lifecycle, password SSH proxy,
// agent connection, and GPU inventory. It intentionally performs no registry
// pull or model download; those external contracts have dedicated cases.
func TestAutoDLNodeOnboarding(t *testing.T) {
	env := setupCloudTest(t, autodlProvider(t), cloudprovider.GPUSpec{GPUModel: "RTX 4080S", GPUCount: 1, MinVRAMMB: 12000})
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "agent_dial" || node.ServerDialAddress != "" {
		t.Fatalf("AutoDL node connection = mode=%q address=%q", node.ConnectMode, node.ServerDialAddress)
	}
	if env.Instance.Kind != "container" {
		t.Fatalf("AutoDL product kind=%q, want container", env.Instance.Kind)
	}
	if len(node.GPUs) == 0 || node.GPUs[0].MemoryTotal < 12000<<20 {
		t.Fatalf("AutoDL GPU inventory = %+v", node.GPUs)
	}
}
