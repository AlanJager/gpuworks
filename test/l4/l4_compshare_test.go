//go:build cloudgpu && compshare

package l4

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// TestCompshareNodeOnboarding proves a live one-GPU VM can be rented,
// connected and observed accurately. Selection walks compshareGPUOrder,
// cheapest first; exact request spelling for all models is covered offline.
func TestCompshareNodeOnboarding(t *testing.T) {
	p := compshareProvider(t)
	// Onboarding checks install, connection and inventory; any card will do.
	spec := cloudprovider.GPUSpec{GPUCount: 1, DiskGB: compshareAcceptanceDiskGB}
	rented, checked := rentCompshareGPU(t, p, spec)
	if rented == nil {
		skipOrFailExternal(t, "no CompShare single-GPU VM could be rented: %s", strings.Join(checked, "; "))
	}
	spec.GPUModel = rented.GPUModel
	env := setupCloudNodeWithInstance(t, testenv.New(t), p, spec, rented)
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	// The normal provider-install path uses agent_dial: the agent reaches the
	// control plane through the verified server forward. Keep server_dial as a
	// separate acceptance contract below, where no server forward is present.
	if node.ConnectMode != "agent_dial" || node.ServerDialAddress != "" {
		t.Fatalf("compshare one-click agent mode = %q address=%q, want agent_dial with no server_dial address", node.ConnectMode, node.ServerDialAddress)
	}
	if env.Instance.Kind != "vm" {
		t.Fatalf("compshare product kind=%q, want vm", env.Instance.Kind)
	}
	if addr, _ := network.ManagementAddress(*node); env.Instance.PrivateIP != "" && addr.IP != env.Instance.PrivateIP {
		t.Fatalf("compshare management address=%q, want private IP %q", addr.IP, env.Instance.PrivateIP)
	}
	if node.RuntimeDriver != "docker" && node.RuntimeDriver != "udocker" {
		t.Fatalf("compshare runtime driver = %q, want docker or udocker", node.RuntimeDriver)
	}
	if len(node.GPUs) != 1 || node.GPUs[0].MemoryTotal <= 0 {
		t.Fatalf("compshare GPU inventory = %+v", node.GPUs)
	}
	out, err := runCloudCommand(t.Context(), env.Instance, "nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>&1 || true")
	if err != nil || !compactGPUStringContains(out, cloudprovider.CompshareGPUDisplayName(spec.GPUModel)) {
		t.Fatalf("compshare SSH GPU check for %s output=%q err=%v", spec.GPUModel, strings.TrimSpace(out), err)
	}
}

// TestCompshareModelScopePublicDownload is the single low-frequency CompShare
// external-download acceptance. It proves the VM can acquire one published
// 8 MiB shard from ModelScope. Product downloader semantics stay in the
// hermetic fixture suite; this case isolates public egress/CDN availability
// and does not pull or start an inference image.
func TestCompshareModelScopePublicDownload(t *testing.T) {
	p := compshareProvider(t)
	// One 8 MiB shard over public egress: no inference, any card will do.
	model := availableCompshareGPU(t, p, 1, 0)
	env := setupCloudTest(t, p, cloudprovider.GPUSpec{GPUModel: model, GPUCount: 1, DiskGB: compshareAcceptanceDiskGB})
	requireOnlineNode(t, env)
	if env.Instance.Kind != "vm" {
		t.Fatalf("CompShare product kind=%q, want vm", env.Instance.Kind)
	}
	verifyCloudPublicShard(t, env, "modelscope", "zstack/qwen3-4b-fake", "main")
}

// TestCompshareDirectServerDialOnboarding keeps the product path honest: the
// installer pushes the binary over SSH and server_dial reaches the public VM
// listener directly, without first making the control plane locally reachable
// through an SSH remote forward.
func TestCompshareDirectServerDialOnboarding(t *testing.T) {
	p := compshareProvider(t)
	env := testenv.New(t)
	instance, checked := rentCompshareGPU(t, p, cloudprovider.GPUSpec{GPUCount: 1, DiskGB: compshareAcceptanceDiskGB})
	if instance == nil {
		skipOrFailExternal(t, "no CompShare single-GPU VM could be rented: %s", strings.Join(checked, "; "))
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := p.Destroy(cleanupCtx, instance.InstanceID); err != nil {
			t.Errorf("destroy instance: %v", err)
		}
	})
	if err := waitCloudSSH(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	name := cloudprovider.TestNamePrefix + "no-forward"
	if err := installCloudAgentViaAPI(t.Context(), t, env, instance, "compshare", name, env.URL(), "server_dial", ":3389", instance.PublicIP); err != nil {
		t.Fatal(err)
	}
	uuid := waitCloudNodeByName(t, env, name)
	env.WaitNodeOnlineUUID(t, uuid)
	node, err := env.Store().NodeWithGPUs(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "server_dial" || node.ServerDialAddress == "" {
		t.Fatalf("direct public server_dial node=%+v", node)
	}
}

// TestCompshareServerDialNodeOnboardingContainer rents CompShare's container
// product, which is reached only through the TCP ports it opens (each
// forwarded from a shared podtcp host, SSH's included), and installs a
// server-dialed agent through the provider API, leaving the port and address
// to the driver: it uses an open port nothing listens on, or opens one with
// UpdateCompShareInstancePorts. The server must then reach the agent at the
// forwarded address, not at the SSH entrance. A VM, which has a public
// address, is covered by TestCompshareDirectServerDialOnboarding.
//
// go test -tags=cloudgpu,compshare -run '^TestCompshareServerDialNodeOnboardingContainer$' -timeout=55m ./test/l4
func TestCompshareServerDialNodeOnboardingContainer(t *testing.T) {
	p := compshareContainerProvider(t)
	instance, checked := rentCompshareContainer(t, p, cloudprovider.GPUSpec{GPUCount: 1, DiskGB: compshareAcceptanceDiskGB, Container: true})
	if instance == nil {
		skipOrFailExternal(t, "no CompShare single-GPU container could be rented: %s", strings.Join(checked, "; "))
	}
	if instance.Kind != "container" {
		t.Fatalf("compshare product kind=%q, want container", instance.Kind)
	}
	env := setupCloudNodeWithInstance(t, testenv.New(t), p, cloudprovider.GPUSpec{GPUModel: instance.GPUModel, GPUCount: 1, Container: true}, instance, withServerDial())
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ConnectMode != "server_dial" || node.ServerURL != "" || node.ServerDialListen == "" || node.ServerDialAddress == "" {
		t.Fatalf("compshare container server_dial node = mode=%q server_url=%q listen=%q address=%q", node.ConnectMode, node.ServerURL, node.ServerDialListen, node.ServerDialAddress)
	}
	// The forwarded address is the container's own entrance for the agent's
	// port, never the SSH one the install went through.
	if sshEndpoint := net.JoinHostPort(instance.PublicIP, strconv.Itoa(instance.SSHPort)); node.ServerDialAddress == sshEndpoint {
		t.Fatalf("server_dial address %s is the SSH endpoint; the agent's own forwarded port was not used", node.ServerDialAddress)
	}
	if len(node.GPUs) != 1 || node.GPUs[0].MemoryTotal <= 0 {
		t.Fatalf("compshare container GPU inventory = %+v", node.GPUs)
	}
}

// TestCompshareCapacityShapes is allocation-free. It records the GPU type
// spelling and every shape returned by the official capacity API, so a live
// test never guesses at inventory that the provider does not expose.
func TestCompshareCapacityShapes(t *testing.T) {
	p := compshareProvider(t)
	for _, model := range []string{"4090", "3080Ti", "3090", "V100S", "P40", "A800", "H20"} {
		t.Run(model, func(t *testing.T) {
			rows, err := checkCompshareCapacity(t, p, model, 120)
			if cloudprovider.IsGPUTypeUnsupportedByImage(err) {
				// The card type exists but not with the image this run uses;
				// that is CompShare's catalog, not a capacity contract to check.
				t.Skipf("CompShare offers %s, but not with this run's image: %v", model, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Fatalf("capacity API returned no specs for %s", model)
			}
			oneGPUShape := false
			oneGPUAvailable := false
			for _, row := range rows {
				t.Logf("GpuType=%s gpu=%d cpu=%d memory_gb=%d resource_enough=%t", model, row.GPU, row.CPU, row.MemoryGB, row.ResourceEnough)
				if row.GPU == 1 {
					oneGPUShape = true
					oneGPUAvailable = oneGPUAvailable || row.ResourceEnough
				}
			}
			if !oneGPUShape {
				t.Fatalf("capacity contract has no one-GPU shape for %s: %+v", model, rows)
			}
			if !oneGPUAvailable {
				t.Logf("no %s one-GPU inventory is currently rentable; shape contract remains valid", model)
			}
		})
	}
}

func checkCompshareCapacity(t *testing.T, p *cloudprovider.Compshare, model string, diskGB int) ([]cloudprovider.CapacitySpec, error) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var rows []cloudprovider.CapacitySpec
		rows, err = p.CheckCapacity(t.Context(), model, diskGB)
		if err == nil || !strings.Contains(err.Error(), "RetCode 8433") {
			return rows, err
		}
		if attempt < 2 {
			t.Logf("capacity API transient error for %s, retrying: %v", model, err)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
	}
	return nil, err
}

// TestCompshareTwoGPUProvisioning verifies that the GPU count of a
// multi-card shape is honored by the actual VM, rather than only checking a
// one-card smoke. It prefers two cards; when no type has a two-card shape in
// stock it takes the smallest wider one any type offers (cn-sh2-02 only ever
// offers 2080Ti x4 and x8), since the count check is the point, not "two".
// Memory per card does not matter here.
func TestCompshareTwoGPUProvisioning(t *testing.T) {
	p := compshareProvider(t)
	var rented *cloudprovider.Instance
	var checked []string
	count := 0
	for _, n := range []int{2, 4, 8} {
		var reasons []string
		rented, reasons = rentCompshareGPU(t, p, cloudprovider.GPUSpec{GPUCount: n, DiskGB: compshareAcceptanceDiskGB})
		if rented != nil {
			count = n
			break
		}
		checked = append(checked, fmt.Sprintf("x%d: %s", n, strings.Join(reasons, "; ")))
	}
	if rented == nil {
		// Multi-card shapes sell out across both VM zones for hours at a time.
		// That is CompShare's stock, not a provisioning defect.
		t.Skipf("no CompShare multi-GPU VM could be rented right now: %s", strings.Join(checked, " | "))
	}
	model := rented.GPUModel
	spec := cloudprovider.GPUSpec{GPUModel: model, GPUCount: count, DiskGB: compshareAcceptanceDiskGB}
	env := setupCloudNodeWithInstance(t, testenv.New(t), p, spec, rented)
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != count {
		t.Fatalf("compshare %s x%d inventory has %d GPUs: %+v", model, count, len(node.GPUs), node.GPUs)
	}
	out, err := runCloudCommand(t.Context(), env.Instance, "nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>&1 || true")
	if err != nil || countGPUCards(out, cloudprovider.CompshareGPUDisplayName(model)) != count {
		t.Fatalf("compshare %s x%d SSH check output=%q err=%v", model, count, strings.TrimSpace(out), err)
	}
}
