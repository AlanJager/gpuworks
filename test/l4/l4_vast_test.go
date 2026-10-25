//go:build cloudgpu && vast

package l4

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// TestVastNodeOnboarding rents the cheapest verified one-GPU offer under the configured
// price ceiling and verifies the real product install path and GPU inventory.
// It intentionally performs no registry pull or model download.
func TestVastNodeOnboarding(t *testing.T) {
	env := setupCloudTest(t, vastProvider(t), cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000, DiskGB: 20})
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "agent_dial" || node.ServerDialAddress != "" {
		t.Fatalf("vast node connection = mode=%q address=%q", node.ConnectMode, node.ServerDialAddress)
	}
	if env.Instance.Kind != "container" {
		t.Fatalf("vast product kind=%q, want container", env.Instance.Kind)
	}
	if len(node.GPUs) == 0 || node.GPUs[0].MemoryTotal < 12000<<20 {
		t.Fatalf("vast GPU inventory = %+v", node.GPUs)
	}
}

// TestVastServerDialNodeOnboarding rents an instance (which the platform
// creates with the agent's port mapped) and installs a server-dialed agent
// through the provider API, leaving the port and address to the driver: the
// server must reach the agent at the public address Vast maps to it.
//
// go test -tags=cloudgpu,vast -run '^TestVastServerDialNodeOnboarding$' -timeout=55m ./test/l4
func TestVastServerDialNodeOnboarding(t *testing.T) {
	env := setupCloudTest(t, vastProvider(t), cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000, DiskGB: 20}, withServerDial())
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	// The instance's SSH endpoint may be one of Vast's proxies (sshN.vast.ai),
	// which holds none of its other ports: the agent is reached at the
	// address Vast maps the instance's ports on.
	vast, ok := env.Provider.(*cloudprovider.Vast)
	if !ok {
		t.Fatalf("provider is %T, want *cloudprovider.Vast", env.Provider)
	}
	host, err := vast.MappedPortHost(t.Context(), env.Instance.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "server_dial" || node.ServerURL != "" || node.ServerDialListen != "0.0.0.0:18800" || !strings.HasPrefix(node.ServerDialAddress, host+":") {
		t.Fatalf("vast server_dial node = mode=%q server_url=%q listen=%q address=%q (want host %s; SSH endpoint %s)", node.ConnectMode, node.ServerURL, node.ServerDialListen, node.ServerDialAddress, host, env.Instance.PublicIP)
	}
}
