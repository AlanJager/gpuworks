//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"bytes"
	"context"
	"crypto/rand"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

const (
	hostedControlPlanePort       = 18880
	hostedBinaryUploadTimeout    = 30 * time.Minute
	hostedPostUploadSetupTimeout = 10 * time.Minute
)

type hostedControlPlaneConfig struct {
	RunID          string
	RuntimeDrivers [2]string
	// Backend is the VM provider; nil means CompShare.
	Backend *vmBackend
	// ModelEndpoint overrides the backend's model mirror.
	ModelEndpoint string
	ServerBinary  string
	AgentBinary   string
	VMCount       int
	// DiskGB overrides the backend's system disk for cases that unpack a large
	// image on the VM itself (udocker keeps a 30+ GiB copy per container).
	DiskGB int
	// OnDemand rents on-demand instead of spot for a case long enough that a
	// reclaim mid-run is likely.
	OnDemand   bool
	SkipAgents bool
	PrepareVMs func(*testing.T, []*cloudTestEnv)
}

type hostedControlPlane struct {
	backend    *vmBackend
	VMs        []*cloudTestEnv
	PrivateURL string
	RemoteRoot string
	API        *hostedAPIClient
	Nodes      []dto.Node
	// gpuModel and gpusPerVM are the live-selected shape the VMs below were
	// rented with; a case needing two cards may run on a wider shape.
	gpuModel  string
	gpusPerVM int
	cleanup   sync.Once
}

func newHostedControlPlane(t *testing.T, config hostedControlPlaneConfig) *hostedControlPlane {
	t.Helper()
	if strings.TrimSpace(config.RunID) == "" {
		t.Fatal("cloud-hosted control plane requires GPUWORKS_CLOUD_TEST_RUN_ID for scoped cleanup")
	}
	serverBinary, agentBinary, err := hostedBinaryPaths(config)
	if err != nil {
		t.Fatalf("hosted control-plane binaries: %v", err)
	}
	backend := config.Backend
	if backend == nil {
		backend = compshareBackend(t)
	}
	vmCount := config.VMCount
	if vmCount == 0 {
		vmCount = 2
	}
	if vmCount < 1 || (!config.SkipAgents && vmCount != 2) {
		t.Fatalf("hosted control-plane VM count=%d skip_agents=%t is unsupported", vmCount, config.SkipAgents)
	}
	gpuModel, gpusPerVM, VMs := rentHostedControlVMs(t, backend, vmCount, config.DiskGB, config.OnDemand)
	for rank, vm := range VMs {
		if vm.Instance.Kind != "vm" || net.ParseIP(vm.Instance.PrivateIP) == nil {
			t.Fatalf("rank %d is not a VM with a valid private address", rank)
		}
	}
	if config.PrepareVMs != nil {
		config.PrepareVMs(t, VMs)
	}
	privateURL, err := hostedPrivateURL(VMs[0].Instance.PrivateIP, hostedControlPlanePort)
	if err != nil {
		t.Fatal(err)
	}
	// Where the server data and every agent's state live. Not under /tmp for a
	// root login: the udocker runtime binds a private directory over the guest's
	// /tmp, which hides the "host path" loopback that lets CUDA consumers find
	// libraries by the host path /proc/self/maps shows, so an engine whose state
	// is under /tmp dies in dlopen of libcudart (see udocker ensureHostRootLoopback).
	// Production state is under /opt/gpuworks, never /tmp.
	root := backend.hostedRoot() + "/gpuworks-hosted-control-" + hostedRunID(config.RunID)
	harness := &hostedControlPlane{backend: backend, gpuModel: gpuModel, gpusPerVM: gpusPerVM, VMs: VMs, PrivateURL: privateURL, RemoteRoot: root}
	t.Cleanup(func() { harness.Close() })
	// Keep the hosted server log available for every subsequent test failure.
	// This cleanup is registered after harness.Close above, so cleanup runs it
	// first while the VMs and server are still reachable.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		diagnosticCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		t.Logf("hosted control-plane diagnostics on failure:\n%s", harness.Diagnostics(diagnosticCtx))
	})

	if err := hostedUploadFileWithin(t.Context(), hostedBinaryUploadTimeout, VMs[0].Instance, serverBinary, root+"/bin/gpuworks", 0o700); err != nil {
		t.Fatalf("upload hosted server binary: %v", err)
	}
	for rank, vm := range VMs {
		if err := hostedUploadFileWithin(t.Context(), hostedBinaryUploadTimeout, vm.Instance, agentBinary, root+"/bin/gpuworks-agent", 0o700); err != nil {
			t.Fatalf("upload rank %d hosted agent binary: %v", rank, err)
		}
	}
	setupCtx, setupCancel := context.WithTimeout(t.Context(), hostedPostUploadSetupTimeout)
	defer setupCancel()

	adminPassword, err := hostedRandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	serverScript, err := hostedServerLaunchScript(hostedServerLaunch{
		Root: root, PrivateIP: VMs[0].Instance.PrivateIP, Port: hostedControlPlanePort,
		AdminPassword: adminPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hostedUploadBytes(setupCtx, VMs[0].Instance, []byte(serverScript), root+"/launch-server.sh", 0o700); err != nil {
		t.Fatalf("upload hosted server launcher: %v", err)
	}
	if out, err := runCloudCommand(setupCtx, VMs[0].Instance, shellQuoteForTest(root+"/launch-server.sh")); err != nil {
		t.Fatalf("start hosted server: %v (%s)", err, strings.TrimSpace(out))
	}
	if err := hostedWaitRemoteHealth(setupCtx, VMs[0].Instance, privateURL); err != nil {
		t.Fatalf("hosted server private health: %v", err)
	}
	// VM0 hosts the control plane. In the normal two-VM topology every peer
	// must prove private reachability before agents or models are started. The
	// one-VM/two-logical-node case intentionally has no remote peer to probe.
	for _, peerRank := range hostedPrivatePeerRanks(len(VMs)) {
		vm := VMs[peerRank]
		if out, err := runCloudCommand(setupCtx, vm.Instance, "curl -fsS --max-time 10 "+shellQuoteForTest(privateURL+"/healthz")+" >/dev/null"); err != nil {
			t.Fatalf("rank %d cannot reach VM1 hosted server over the private network: %v (%s)", peerRank, err, strings.TrimSpace(out))
		}
	}

	forwardFactory := hostedSSHForwardFactory(VMs[0].Instance, net.JoinHostPort(VMs[0].Instance.PrivateIP, strconv.Itoa(hostedControlPlanePort)))
	harness.API = newHostedAPIClient(forwardFactory, "admin", adminPassword)
	var health dto.Health
	if err := harness.API.GetJSON(t.Context(), "/api/v1/healthz", &health); err != nil || health.Status != "ok" {
		t.Fatalf("authenticate hosted control plane: health=%+v err=%v", health, err)
	}
	endpoint := strings.TrimSpace(config.ModelEndpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(backend.ModelEndpoint)
	}
	if endpoint != "" {
		// A model mirror is a site rule, set the way an operator does.
		allowUpstream := true
		rule := dto.PullSourceRuleRequest{Match: "https://huggingface.co", Endpoints: []string{strings.TrimRight(endpoint, "/")}, AllowUpstream: &allowUpstream}
		if err := harness.API.PostJSON(t.Context(), "/api/v1/pull/source-rules", rule, nil); err != nil {
			t.Fatalf("configure the hosted model mirror: %v", err)
		}
	}
	if config.SkipAgents {
		return harness
	}

	harness.Nodes = make([]dto.Node, 2)
	for rank, vm := range VMs {
		driver := strings.TrimSpace(config.RuntimeDrivers[rank])
		if driver == "" {
			driver = "udocker"
		}
		created, token, err := hostedCreateNode(t.Context(), harness.API, fmt.Sprintf("hosted-rank%d", rank), privateURL)
		if err != nil {
			t.Fatalf("create hosted rank %d: %v", rank, err)
		}
		agentScript, err := hostedAgentLaunchScript(hostedAgentLaunch{
			Root: root, Rank: rank, ServerURL: privateURL, Token: token,
			RuntimeDriver: driver,
		})
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("%s/launch-agent-rank%d.sh", root, rank)
		if err := hostedUploadBytes(setupCtx, vm.Instance, []byte(agentScript), path, 0o700); err != nil {
			t.Fatalf("upload rank %d agent launcher: %v", rank, err)
		}
		if out, err := runCloudCommand(setupCtx, vm.Instance, shellQuoteForTest(path)); err != nil {
			t.Fatalf("start hosted rank %d agent: %v (%s)", rank, err, strings.TrimSpace(out))
		}
		harness.Nodes[rank] = created
	}
	harness.Nodes = hostedWaitNodes(t, harness, 2, 4*time.Minute)
	return harness
}

func hostedPrivatePeerRanks(vmCount int) []int {
	if vmCount <= 1 {
		return nil
	}
	ranks := make([]int, 0, vmCount-1)
	for rank := 1; rank < vmCount; rank++ {
		ranks = append(ranks, rank)
	}
	return ranks
}

// rentHostedControlVMs rents vmCount VMs of one shape with at least two cards
// each, trying the next shape when the provider refuses the create for lack of
// capacity: the stock check that picked the shape is only a snapshot, and
// both VMs have to come from the same shape. VMs already rented for a shape
// that then runs out are destroyed before the next one is tried.
func rentHostedControlVMs(t *testing.T, backend *vmBackend, vmCount, diskGB int, onDemand bool) (string, int, []*cloudTestEnv) {
	t.Helper()
	var (
		gpuModel string
		gpus     int
		rented   []*cloudTestEnv
	)
	checked := backend.EachShape(t, true, 2, hostedMinVRAMMB, func(model string, count int) bool {
		var envs []*cloudTestEnv
		for len(envs) < vmCount {
			env, err := rentHostedControlVM(t, backend, model, count, diskGB, onDemand)
			if err == nil {
				envs = append(envs, env)
				continue
			}
			if !isCloudCapacityError(err) {
				t.Fatal(err)
			}
			t.Logf("%s %s x%d ran out after %d of %d VMs, trying the next shape: %v", backend.Name, model, count, len(envs), vmCount, err)
			for _, env := range envs {
				destroyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := env.cleanup(destroyCtx); err != nil {
					t.Errorf("destroy hosted control-plane VM of an abandoned shape: %v", err)
				}
				cancel()
			}
			return false
		}
		gpuModel, gpus, rented = model, count, envs
		return true
	})
	if rented == nil {
		t.Fatalf("check %s capacity for two or more GPUs: %s", backend.Name, strings.Join(checked, "; "))
	}
	return gpuModel, gpus, rented
}

// rentHostedControlVM rents one VM of the shape and waits for SSH. A refused
// create comes back as an error for the caller to judge; the VM, once rented,
// is destroyed when the test ends.
func rentHostedControlVM(t *testing.T, backend *vmBackend, gpuModel string, gpus, diskGB int, onDemand bool) (*cloudTestEnv, error) {
	if diskGB <= 0 {
		diskGB = backend.DiskGB
	}
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), cloudSetupTimeout)
	defer cancel()
	instance, err := backend.Provider.Rent(ctx, cloudprovider.GPUSpec{GPUModel: gpuModel, GPUCount: gpus, MinVRAMMB: hostedMinVRAMMB, DiskGB: diskGB, OnDemand: onDemand})
	if err != nil {
		return nil, err
	}
	logRentedInstance(t, backend, instance)
	env := &cloudTestEnv{Provider: backend.Provider, Instance: instance}
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("GPUWORKS_CLOUD_KEEP_ON_FAILURE") == "1" {
			// Same contract as the single-VM cases: leave the failed VM for
			// investigation and say how to get in and how to release it.
			path, credentialErr := saveCloudDebugCredential(backend.Provider, instance)
			t.Logf("GPUWORKS_CLOUD_KEEP_ON_FAILURE=1: leaving VM %s (%s) running; SSH %s@%s:%d, credentials file %s (0600, err %v); release it with cleanup-leaked --run-id %s",
				instance.InstanceID, backend.Name, instance.Username, instance.PublicIP, instance.SSHPort, path, credentialErr, cloudProviderDebugRunID(backend.Provider))
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := env.cleanup(cleanupCtx); err != nil {
			t.Errorf("destroy hosted control-plane VM: %v", err)
		}
	})
	// Registered after the destroy above so it runs first: a reclaimed spot VM
	// must be named as such before the instance is gone for good.
	t.Cleanup(func() { reportInterruption(t, backend.Provider, instance) })
	if err := waitCloudSSH(ctx, instance); err != nil {
		t.Fatal(err)
	}
	return env, nil
}

func hostedCreateNode(ctx context.Context, api *hostedAPIClient, name, serverURL string) (dto.Node, string, error) {
	var response struct {
		Node  dto.Node `json:"node"`
		Token string   `json:"token"`
	}
	err := api.PostJSON(ctx, "/api/v1/nodes", map[string]any{
		"name": name, "connect_mode": "agent_dial", "server_url": serverURL,
	}, &response)
	if err != nil {
		return dto.Node{}, "", err
	}
	if response.Node.UUID == "" || response.Token == "" {
		return dto.Node{}, "", errors.New("create hosted node returned an empty UUID or token")
	}
	return response.Node, response.Token, nil
}

func hostedWaitNodes(t *testing.T, harness *hostedControlPlane, expected int, timeout time.Duration) []dto.Node {
	return hostedWaitNodesWithGPUCount(t, harness, expected, harness.gpusPerVM, timeout)
}

// hostedWithSchedulableGPUs narrows a node to the cards work can be placed
// on. The one-VM logical-node case gives each agent one card by hiding the
// other from nvidia-smi, but the agent also reads the PCI bus and reports the
// hidden card as present and unusable (no index, driver_init_failed): the
// node honestly has two cards, one of which is not its to use. Counting the
// schedulable ones keeps every other case unchanged, since all their cards
// are schedulable.
func hostedWithSchedulableGPUs(node dto.Node) dto.Node {
	usable := make([]dto.GPU, 0, len(node.GPUs))
	for _, gpu := range node.GPUs {
		if gpu.Schedulable {
			usable = append(usable, gpu)
		}
	}
	node.GPUs = usable
	node.GPUCount = len(usable)
	return node
}

func hostedWaitNodesWithGPUCount(t *testing.T, harness *hostedControlPlane, expected, expectedGPUs int, timeout time.Duration) []dto.Node {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var nodes []dto.Node
	var lastErr error
	for time.Now().Before(deadline) {
		var listed apiwire.List[dto.Node]
		lastErr = harness.API.GetJSON(t.Context(), "/api/v1/nodes", &listed)
		nodes = listed.Items
		if lastErr == nil && len(nodes) == expected {
			allOnline := true
			for i := range nodes {
				nodes[i] = hostedWithSchedulableGPUs(nodes[i])
				if nodes[i].State != "online" || nodes[i].GPUCount != expectedGPUs || len(nodes[i].GPUs) != expectedGPUs {
					allOnline = false
				}
			}
			if allOnline {
				sortHostedNodes(nodes)
				return nodes
			}
		}
		time.Sleep(time.Second)
	}
	diagnostic := harness.Diagnostics(t.Context())
	t.Fatalf("hosted control plane did not report %d online nodes with %d GPUs each: nodes=%+v err=%v\n%s", expected, expectedGPUs, nodes, lastErr, diagnostic)
	return nil
}

func sortHostedNodes(nodes []dto.Node) {
	if len(nodes) == 2 && nodes[0].Name > nodes[1].Name {
		nodes[0], nodes[1] = nodes[1], nodes[0]
	}
}

func (h *hostedControlPlane) DropForward() {
	if h != nil && h.API != nil {
		h.API.DropForward()
	}
}

func (h *hostedControlPlane) Close() {
	if h == nil {
		return
	}
	h.cleanup.Do(func() {
		if h.API != nil {
			h.API.Close()
		}
		for rank, vm := range h.VMs {
			if vm == nil || vm.Instance == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = runCloudCommand(ctx, vm.Instance, hostedStopCommand(h.RemoteRoot, rank == 0))
			cancel()
		}
	})
}

func (h *hostedControlPlane) Diagnostics(ctx context.Context) string {
	if h == nil {
		return "hosted harness is nil"
	}
	var diagnostics []string
	for rank, vm := range h.VMs {
		if vm == nil || vm.Instance == nil {
			continue
		}
		command := fmt.Sprintf(`set +e
echo rank=%d
for pidfile in %s/*.pid; do [ -f "$pidfile" ] || continue; pid="$(cat "$pidfile")"; ps -p "$pid" -o pid=,comm=; done
ss -ltn 2>/dev/null | grep -E ':%d[[:space:]]' || true
if [ -f %s/log/server.log ]; then
  echo server-log:
  tail -n 200 %s/log/server.log
fi
tail -n 100 %s/log/agent-*.log 2>/dev/null || true
# A chat that fails with an engine error says nothing about why: the stack
# trace is in the engine container's own log.
docker_cmd=docker; docker info >/dev/null 2>&1 || docker_cmd='sudo -n docker'
for c in $($docker_cmd ps -aq 2>/dev/null); do
  echo "engine-container $($docker_cmd inspect --format '{{.Name}} {{.Config.Image}} {{.State.Status}} exit={{.State.ExitCode}}' "$c" 2>/dev/null):"
  $docker_cmd logs --tail 60 "$c" 2>&1
done`, rank, shellQuoteForTest(h.RemoteRoot), hostedControlPlanePort,
			shellQuoteForTest(h.RemoteRoot), shellQuoteForTest(h.RemoteRoot), shellQuoteForTest(h.RemoteRoot))
		out, err := runCloudCommand(ctx, vm.Instance, command)
		diagnostics = append(diagnostics, fmt.Sprintf("rank=%d diagnostic_err=%v\n%s", rank, err, strings.TrimSpace(out)))
	}
	return strings.Join(diagnostics, "\n---\n")
}

func hostedStopCommand(root string, stopServer bool) string {
	pidFiles := []string{fmt.Sprintf("%s/agent-rank*.pid", root)}
	if stopServer {
		pidFiles = append(pidFiles, root+"/server.pid")
	}
	return fmt.Sprintf(`set +e
for pidfile in %s; do
  [ -s "$pidfile" ] || continue
  pid="$(cat "$pidfile")"
  case "$pid" in ''|*[!0-9]*) continue;; esac
  kill "$pid" 2>/dev/null || true
done`, strings.Join(pidFiles, " "))
}

func hostedPrivateURL(privateIP string, port int) (string, error) {
	privateIP = strings.TrimSpace(privateIP)
	parsed := net.ParseIP(privateIP)
	if parsed == nil || !parsed.IsPrivate() || parsed.IsUnspecified() || parsed.IsLoopback() || port < 1024 || port > 65535 {
		return "", fmt.Errorf("invalid hosted private endpoint")
	}
	return "http://" + net.JoinHostPort(privateIP, strconv.Itoa(port)), nil
}

func hostedRunID(runID string) string {
	clean := regexp.MustCompile(`[^a-zA-Z0-9_.-]+`).ReplaceAllString(strings.TrimSpace(runID), "-")
	clean = strings.Trim(clean, ".-")
	if clean == "" {
		return "invalid"
	}
	return clean
}

func hostedRandomSecret() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func hostedUploadFile(ctx context.Context, instance *cloudprovider.Instance, local, remote string, mode os.FileMode) error {
	if instance.Password != "" {
		return sshutil.UploadFilePassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, local, remote, mode)
	}
	return sshutil.UploadFile(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, local, remote, mode)
}

func hostedUploadFileWithin(parent context.Context, timeout time.Duration, instance *cloudprovider.Instance, local, remote string, mode os.FileMode) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return hostedUploadFile(ctx, instance, local, remote, mode)
}

func hostedUploadBytes(ctx context.Context, instance *cloudprovider.Instance, data []byte, remote string, mode os.FileMode) error {
	if instance.Password != "" {
		return sshutil.UploadBytesPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, data, remote, mode)
	}
	return sshutil.UploadBytes(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, data, remote, mode)
}

func hostedBinaryPaths(config hostedControlPlaneConfig) (string, string, error) {
	root, err := hostedRepoRoot()
	if err != nil {
		return "", "", err
	}
	server := strings.TrimSpace(config.ServerBinary)
	if server == "" {
		server = filepath.Join(root, "bin", "gpuworks-linux-amd64")
	}
	agent := strings.TrimSpace(config.AgentBinary)
	if agent == "" {
		agent = filepath.Join(root, "bin", "dl", "gpuworks-agent-linux-amd64")
	}
	for _, binary := range []string{server, agent} {
		if err := validateHostedStaticAMD64(binary); err != nil {
			return "", "", err
		}
	}
	return server, agent, nil
}

func validateHostedStaticAMD64(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("missing binary %s; run make server-linux-amd64: %w", path, err)
	}
	file, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("open ELF binary %s: %w", path, err)
	}
	defer file.Close()
	if file.Machine != elf.EM_X86_64 {
		return fmt.Errorf("binary %s is %s, want x86-64", path, file.Machine)
	}
	imports, err := file.ImportedLibraries()
	if err == nil && len(imports) > 0 {
		return fmt.Errorf("binary %s is dynamically linked: %v", path, imports)
	}
	return nil
}

func hostedRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repository root containing go.mod was not found")
		}
		dir = parent
	}
}

type hostedServerLaunch struct {
	Root, PrivateIP, AdminPassword string
	Port                           int
}

func hostedServerLaunchScript(config hostedServerLaunch) (string, error) {
	privateURL, err := hostedPrivateURL(config.PrivateIP, config.Port)
	if err != nil || strings.TrimSpace(config.Root) == "" || strings.TrimSpace(config.AdminPassword) == "" {
		return "", errors.New("invalid hosted server launch configuration")
	}
	return fmt.Sprintf(`#!/bin/sh
set -eu
root=%s
mkdir -p "$root/data" "$root/log"
if [ -s "$root/server.pid" ]; then kill "$(cat "$root/server.pid")" 2>/dev/null || true; fi
# The first start creates the admin account from this file, as after an install.
umask 077
printf '{"user":"admin","password":"%%s"}\n' %s >"$root/data/initial-credentials"
nohup "$root/bin/gpuworks" serve --addr %s --data "$root/data" --recovery-enabled=false >"$root/log/server.log" 2>&1 </dev/null &
pid=$!
printf '%%s\n' "$pid" > "$root/server.pid"
for attempt in $(seq 1 120); do
  if curl -fsS --max-time 2 %s/healthz >/dev/null; then exit 0; fi
  if ! kill -0 "$pid" 2>/dev/null; then tail -n 100 "$root/log/server.log" >&2; exit 1; fi
  sleep 1
done
exit 1
`, shellQuoteForTest(config.Root), shellQuoteForTest(config.AdminPassword), shellQuoteForTest(net.JoinHostPort(config.PrivateIP, strconv.Itoa(config.Port))), privateURL), nil
}

type hostedAgentLaunch struct {
	Root, ServerURL, Token, RuntimeDriver string
	Rank                                  int
}

func hostedAgentLaunchScript(config hostedAgentLaunch) (string, error) {
	if config.Rank < 0 || config.Rank > 1 || strings.TrimSpace(config.Root) == "" || strings.TrimSpace(config.Token) == "" {
		return "", errors.New("invalid hosted agent launch configuration")
	}
	serverURL, parseErr := url.Parse(config.ServerURL)
	host, port, splitErr := net.SplitHostPort(serverURL.Host)
	if parseErr != nil || serverURL.Scheme != "http" || serverURL.Path != "" || splitErr != nil || port != strconv.Itoa(hostedControlPlanePort) || net.ParseIP(host) == nil {
		return "", errors.New("hosted agent server URL must use the VM private endpoint")
	}
	state := fmt.Sprintf("%s/agent-rank%d", config.Root, config.Rank)
	return fmt.Sprintf(`#!/bin/sh
set -eu
root=%s
state=%s
mkdir -p "$state" "$root/log"
# Upload creates $root with umask 077. An agent that logs in as root runs udocker
# workloads as the dedicated runtime user, which must be able to traverse the
# directory above its state to reach the PRoot binary ("fork/exec .../proot-x86_64:
# permission denied" on Aliyun otherwise). Traverse only: nothing is listable.
chmod 711 "$root"
if [ -s "$root/agent-rank%d.pid" ]; then kill "$(cat "$root/agent-rank%d.pid")" 2>/dev/null || true; fi
export GPUWORKS_SERVER=%s
export GPUWORKS_TOKEN=%s
export GPUWORKS_RUNTIME_DRIVER=%s
nohup "$root/bin/gpuworks-agent" run --state "$state" --connect-mode agent_dial >"$root/log/agent-rank%d.log" 2>&1 </dev/null &
pid=$!
printf '%%s\n' "$pid" > "$root/agent-rank%d.pid"
`, shellQuoteForTest(config.Root), shellQuoteForTest(state), config.Rank, config.Rank,
		shellQuoteForTest(config.ServerURL), shellQuoteForTest(config.Token), shellQuoteForTest(config.RuntimeDriver),
		config.Rank, config.Rank), nil
}

func hostedWaitRemoteHealth(ctx context.Context, instance *cloudprovider.Instance, serverURL string) error {
	out, err := runCloudCommand(ctx, instance, "curl -fsS --max-time 10 "+shellQuoteForTest(serverURL+"/healthz"))
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	if !bytes.Contains([]byte(out), []byte(`"status":"ok"`)) {
		return fmt.Errorf("health response did not report ok")
	}
	return nil
}
