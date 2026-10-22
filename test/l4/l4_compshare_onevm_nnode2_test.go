//go:build cloudgpu && compshare

package l4

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

type compshareOneVMLogicalNode struct {
	Rank     int
	Name     string
	UUID     string
	Token    string
	GPU      compsharePhysicalGPU
	StateDir string
	Owner    string
	APINode  dto.Node
}

// TestCompshareOneVMTwoLogicalNodeDistributedInference is a product-level distributed
// acceptance case, not a physical multi-VM claim. It runs two independent
// gpuworks agents on one real two-GPU CompShare VM. Each logical node sees one
// different physical GPU and owns a disjoint Docker container namespace. The
// deployment itself still travels through the normal gpuworks API/controller,
// starts two real vLLM engine containers with nnodes=2, and serves gateway chat.
// The GPUWorks server also runs on this VM; both agents use its private address,
// while the workstation SSH forward carries only REST/gateway requests.
func TestCompshareOneVMTwoLogicalNodeDistributedInference(t *testing.T) {
	var physical []compsharePhysicalGPU
	harness := newHostedControlPlane(t, hostedControlPlaneConfig{
		RunID: cloudTestRunID(), VMCount: 1, SkipAgents: true,
		ModelEndpoint: compshareModelEndpoint(),
		PrepareVMs: func(t *testing.T, envs []*cloudTestEnv) {
			installCompshareDocker29OnNodes(t, envs)
			prepareCompshareDockerImageCache(t, envs)
			installDistributedVLLMFix(t, envs)
			if !t.Run("DockerNCCL", func(t *testing.T) { testCompshareDockerNCCL(t, envs[0]) }) {
				t.FailNow()
			}
			physical = readCompsharePhysicalGPUs(t, envs[0])
		},
	})
	base := harness.VMs[0]
	// Two logical nodes take one card each; a wider VM keeps the rest idle.
	if len(physical) < 2 || physical[0].UUID == physical[1].UUID {
		t.Fatalf("CompShare physical GPU inventory has no two distinct devices: %+v", physical)
	}
	physical = physical[:2]
	serverURL := harness.PrivateURL
	directAddress := strings.TrimSpace(base.Instance.PrivateIP)
	if directAddress == "" {
		t.Fatal("one-VM logical-node case requires the VM private address for rendezvous")
	}

	nodes := make([]compshareOneVMLogicalNode, 2)
	logicalRoot := harness.RemoteRoot + "/onevm-logical"
	for rank := range nodes {
		name := fmt.Sprintf("hosted-onevm-rank%d", rank)
		created, token, err := hostedCreateNode(t.Context(), harness.API, name, serverURL)
		if err != nil {
			t.Fatalf("create one-VM logical rank %d: %v", rank, err)
		}
		nodes[rank] = compshareOneVMLogicalNode{
			Rank: rank, Name: name, UUID: created.UUID, Token: token, GPU: physical[rank],
			StateDir: fmt.Sprintf("%s/rank%d", logicalRoot, rank),
			Owner:    fmt.Sprintf("%s-rank%d", hostedRunID(cloudTestRunID()), rank),
		}
	}
	cleanupOneVMLogicalAgents := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = runCloudCommand(ctx, base.Instance, oneVMLogicalCleanupCommand(logicalRoot, [2]string{nodes[0].Owner, nodes[1].Owner}))
	}
	t.Cleanup(cleanupOneVMLogicalAgents)

	for i := range nodes {
		installOneVMLogicalAgent(t, base, harness.RemoteRoot+"/bin/gpuworks-agent", serverURL, directAddress, nodes[i].GPU.Index, nodes[i].Owner, nodes[i].StateDir, nodes[i].Token)
	}
	harness.Nodes = hostedWaitNodesWithGPUCount(t, harness, 2, 1, 4*time.Minute)
	byUUID := make(map[string]dto.Node, len(harness.Nodes))
	for _, node := range harness.Nodes {
		byUUID[node.UUID] = node
	}
	for i := range nodes {
		nodes[i].APINode = byUUID[nodes[i].UUID]
		assertOneVMLogicalGPU(t, &nodes[i].APINode, nodes[i].GPU)
	}
	if nodes[0].APINode.GPUs[0].UUID == nodes[1].APINode.GPUs[0].UUID {
		t.Fatalf("logical nodes reported the same physical GPU UUID: %s", nodes[0].APINode.GPUs[0].UUID)
	}
	t.Logf("one physical VM now has two logical gpuworks nodes with distinct GPUs: rank0=index%d rank1=index%d", nodes[0].GPU.Index, nodes[1].GPU.Index)

	model := hostedPrepareRealModel(t, harness)
	d := createHostedDeployment(t, harness, map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID,
		"node_uuids":  []string{nodes[0].UUID, nodes[1].UUID},
		"served_name": "qwen2-compshare-onevm-nnode2", "engine": "vllm",
		"image": compshareDistributedTestImage(), "nnodes": 2,
		"master_addr": directAddress, "master_port": 29500,
		"placement":           map[string]any{"tp": 1, "pp": 2},
		"quick_params":        map[string]any{"max_model_len": 1024},
		"container_overrides": distributedVLLMContainerOverrides(),
	}, 2, 75*time.Minute)

	assertOneVMDistributedInstances(t, base, d.instances, nodes)
	d.models(t)
	d.chat(t)
	t.Log("one-VM/two-logical-node nnodes=2 gateway chat passed with two real vLLM containers")
	d.stop(t)
}

type compsharePhysicalGPU struct {
	Index int
	UUID  string
}

func readCompsharePhysicalGPUs(t *testing.T, env *cloudTestEnv) []compsharePhysicalGPU {
	t.Helper()
	out, err := runCloudCommand(t.Context(), env.Instance, "nvidia-smi --query-gpu=index,uuid --format=csv,noheader,nounits")
	if err != nil {
		t.Fatalf("query physical CompShare GPUs: %v", err)
	}
	var result []compsharePhysicalGPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		index, parseErr := strconv.Atoi(strings.TrimSpace(fields[0]))
		if parseErr != nil || strings.TrimSpace(fields[1]) == "" {
			continue
		}
		result = append(result, compsharePhysicalGPU{Index: index, UUID: strings.TrimSpace(fields[1])})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })
	return result
}

func installOneVMLogicalAgent(t *testing.T, env *cloudTestEnv, agentBinary, serverURL, directAddress string, physicalGPUIndex int, owner, stateDir, token string) {
	t.Helper()
	binDir := filepath.Join(stateDir, "bin")
	dockerWrapper := oneVMLogicalDockerWrapper("/usr/bin/docker", owner, physicalGPUIndex)
	nvidiaWrapper := oneVMLogicalNVIDIAWrapper("/usr/bin/nvidia-smi", physicalGPUIndex)
	launcher := fmt.Sprintf(`#!/bin/sh
set -eu
export PATH=%s:/usr/bin:/bin
export GPUWORKS_SERVER=%s
export GPUWORKS_TOKEN=%s
exec %s run --state %s --driver docker
`, shellQuoteForTest(binDir), shellQuoteForTest(serverURL), shellQuoteForTest(token), shellQuoteForTest(agentBinary), shellQuoteForTest(stateDir))

	files := map[string]string{
		filepath.Join(binDir, "docker"):      dockerWrapper,
		filepath.Join(binDir, "nvidia-smi"):  nvidiaWrapper,
		filepath.Join(stateDir, "launch.sh"): launcher,
	}
	var setup strings.Builder
	setup.WriteString("set -eu\nroot=''; if [ \"$(id -u)\" -ne 0 ]; then root='sudo -n'; fi\n")
	setup.WriteString("$root mkdir -p " + shellQuoteForTest(binDir) + " " + shellQuoteForTest(filepath.Join(stateDir, "models")) + "\n")
	for path, contents := range files {
		encoded := base64.StdEncoding.EncodeToString([]byte(contents))
		setup.WriteString("printf %s " + shellQuoteForTest(encoded) + " | base64 -d | $root tee " + shellQuoteForTest(path) + " >/dev/null\n")
		setup.WriteString("$root chmod 700 " + shellQuoteForTest(path) + "\n")
	}
	setup.WriteString("$root chown -R \"$(id -u):$(id -g)\" " + shellQuoteForTest(stateDir) + "\n")
	// Prove the wrapper selects the intended physical index before trusting it
	// as the inventory source of a long-running agent.
	setup.WriteString("selected=$($root env PATH=" + shellQuoteForTest(binDir+":/usr/bin:/bin") + " nvidia-smi --query-gpu=index --format=csv,noheader,nounits | tr -d '[:space:]')\n")
	setup.WriteString("[ \"$selected\" = " + shellQuoteForTest(strconv.Itoa(physicalGPUIndex)) + " ]\n")
	launchCommand := "nohup " + shellQuoteForTest(filepath.Join(stateDir, "launch.sh")) + " > " + shellQuoteForTest(filepath.Join(stateDir, "stdout.log")) + " 2>&1 </dev/null & echo $! > " + shellQuoteForTest(filepath.Join(stateDir, "agent.pid"))
	setup.WriteString("sh -c " + shellQuoteForTest(launchCommand) + "\n")
	if out, err := runCloudCommand(t.Context(), env.Instance, setup.String()); err != nil {
		t.Fatalf("start logical GPU index %d agent: %v (%s)", physicalGPUIndex, err, strings.TrimSpace(out))
	}
}

func oneVMLogicalDockerWrapper(realDocker, owner string, physicalGPUIndex int) string {
	return fmt.Sprintf(`#!/bin/sh
	set -eu
	real=%s
	owner=%s
if [ "${1:-}" = run ]; then
	shift
	exec /bin/bash -c '
	    real="$1"; owner="$2"; physical="$3"; shift 3
	    args=()
	    for arg in "$@"; do
	      if [[ "$arg" == "CUDA_VISIBLE_DEVICES=$physical" ]]; then
	        arg="CUDA_VISIBLE_DEVICES=0"
	      fi
	      args+=("$arg")
	    done
	    exec "$real" run --label "gpuworks.logical-node=$owner" "${args[@]}"
	  ' logical-run "$real" "$owner" %d "$@"
	fi
if [ "${1:-}" = ps ]; then
  shift
  exec "$real" ps --filter "label=gpuworks.logical-node=$owner" "$@"
fi
exec "$real" "$@"
	`, shellQuoteForTest(realDocker), shellQuoteForTest(owner), physicalGPUIndex)
}

func oneVMLogicalNVIDIAWrapper(realSMI string, physicalIndex int) string {
	return fmt.Sprintf(`#!/bin/sh
	set -eu
	if [ "${1:-}" = dmon ]; then
	  shift
	  exec %s dmon -i %d "$@"
	fi
	exec %s -i %d "$@"
`, shellQuoteForTest(realSMI), physicalIndex, shellQuoteForTest(realSMI), physicalIndex)
}

func assertOneVMLogicalGPU(t *testing.T, node *dto.Node, want compsharePhysicalGPU) {
	t.Helper()
	if node == nil || node.State != "online" || node.RuntimeDriver != "docker" || len(node.GPUs) != 1 {
		t.Fatalf("logical node inventory=%+v, want exactly one GPU", node)
	}
	got := node.GPUs[0]
	if got.Index != want.Index || got.UUID != want.UUID || !got.Schedulable {
		t.Fatalf("logical node GPU index/UUID/schedulable=%d/%s/%v, want %d/%s/true", got.Index, got.UUID, got.Schedulable, want.Index, want.UUID)
	}
}

func assertOneVMDistributedInstances(t *testing.T, env *cloudTestEnv, instances []dto.Instance, nodes []compshareOneVMLogicalNode) {
	t.Helper()
	if len(instances) != 2 {
		t.Fatalf("distributed instances=%d, want 2", len(instances))
	}
	byNodeUUID := map[string]int{}
	for i := range nodes {
		byNodeUUID[nodes[i].UUID] = i
	}
	seenRanks := map[int]bool{}
	seenGPUUUIDs := map[string]bool{}
	for _, instance := range instances {
		nodeIndex, ok := byNodeUUID[instance.NodeUUID]
		if !ok {
			t.Fatalf("instance %s belongs to unexpected node UUID %s", instance.UUID, instance.NodeUUID)
		}
		node := nodes[nodeIndex]
		if instance.NNodes != 2 || instance.NodeRank != node.Rank || len(instance.GPUIndexes) != 1 || instance.GPUIndexes[0] != node.GPU.Index {
			t.Fatalf("rank %d distributed instance=%+v, want nnodes=2 and physical GPU index %d", node.Rank, instance, node.GPU.Index)
		}
		for _, required := range []string{"--nnodes", "2", "--node-rank", strconv.Itoa(node.Rank), "--master-port", "29500"} {
			if !containsString(instance.Argv, required) {
				t.Fatalf("rank %d missing distributed arg %q in %v", node.Rank, required, instance.Argv)
			}
		}
		seenRanks[instance.NodeRank] = true
		if seenGPUUUIDs[node.GPU.UUID] {
			t.Fatalf("deployment reused physical GPU UUID %s", node.GPU.UUID)
		}
		seenGPUUUIDs[node.GPU.UUID] = true

		inspect := fmt.Sprintf("docker_cmd=docker; docker info >/dev/null 2>&1 || docker_cmd='sudo -n docker'; $docker_cmd inspect --format '{{index .Config.Labels \"gpuworks.logical-node\"}}' %s && $docker_cmd exec %s nvidia-smi --query-gpu=uuid --format=csv,noheader,nounits", shellQuoteForTest(instance.ContainerID), shellQuoteForTest(instance.ContainerID))
		out, err := runCloudCommand(t.Context(), env.Instance, inspect)
		if err != nil {
			t.Fatalf("inspect rank %d vLLM container: %v", node.Rank, err)
		}
		lines := strings.Fields(out)
		if len(lines) < 2 || lines[0] != node.Owner || lines[len(lines)-1] != node.GPU.UUID {
			t.Fatalf("rank %d container label/GPU evidence=%q, want %q/%q", node.Rank, strings.TrimSpace(out), node.Owner, node.GPU.UUID)
		}
	}
	if !seenRanks[0] || !seenRanks[1] || len(seenGPUUUIDs) != 2 {
		t.Fatalf("distributed evidence ranks=%v GPU UUIDs=%v", seenRanks, seenGPUUUIDs)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func oneVMLogicalCleanupCommand(root string, owners [2]string) string {
	return fmt.Sprintf(`set +e
priv=""; if [ "$(id -u)" -ne 0 ]; then priv="sudo -n"; fi
for state in %s/rank0 %s/rank1; do
  if [ -s "$state/agent.pid" ]; then $priv kill "$(cat "$state/agent.pid")" 2>/dev/null || true; fi
done
for owner in %s %s; do
  ids="$($priv docker ps -aq --filter "label=gpuworks.logical-node=$owner" 2>/dev/null)"
  if [ -n "$ids" ]; then $priv docker rm -f $ids >/dev/null 2>&1 || true; fi
done
`, shellQuoteForTest(root), shellQuoteForTest(root), shellQuoteForTest(owners[0]), shellQuoteForTest(owners[1]))
}

// TestOneVMLogicalDockerWrapperScopesOrphanListing is hermetic proof that the
// wrapper both labels this logical node's runs and hides the sibling node from
// the docker ps call used by Engine.CleanupOrphans.
func TestOneVMLogicalDockerWrapperScopesOrphanListing(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	fakeDocker := filepath.Join(dir, "docker-real")
	wrapper := filepath.Join(dir, "docker")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuoteForTest(logPath) + "\n"
	if err := os.WriteFile(fakeDocker, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte(oneVMLogicalDockerWrapper(fakeDocker, "rank1", 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"run", "-e", "CUDA_VISIBLE_DEVICES=1", "-d", "image"}, {"ps", "-a", "--format", "{{.Names}}"}, {"inspect", "container"}} {
		if out, err := exec.Command(wrapper, args...).CombinedOutput(); err != nil {
			t.Fatalf("wrapper %v: %v (%s)", args, err, out)
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"run --label gpuworks.logical-node=rank1 -e CUDA_VISIBLE_DEVICES=0 -d image",
		"ps --filter label=gpuworks.logical-node=rank1 -a --format {{.Names}}",
		"inspect container",
	}
	if len(lines) != len(want) {
		t.Fatalf("docker wrapper calls=%q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("docker wrapper call %d=%q, want %q", i, lines[i], want[i])
		}
	}
}

func TestOneVMLogicalNVIDIAWrapperPinsPhysicalIndex(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	fakeSMI := filepath.Join(dir, "nvidia-smi-real")
	wrapper := filepath.Join(dir, "nvidia-smi")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuoteForTest(logPath) + "\n"
	if err := os.WriteFile(fakeSMI, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte(oneVMLogicalNVIDIAWrapper(fakeSMI, 7)), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "--query-gpu=index,uuid", "--format=csv,noheader,nounits").CombinedOutput(); err != nil {
		t.Fatalf("NVIDIA wrapper: %v (%s)", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "-i 7 --query-gpu=index,uuid --format=csv,noheader,nounits"; got != want {
		t.Fatalf("NVIDIA wrapper call=%q, want %q", got, want)
	}
	if out, err := exec.Command(wrapper, "dmon", "-c", "1", "-s", "t").CombinedOutput(); err != nil {
		t.Fatalf("NVIDIA dmon wrapper: %v (%s)", err, out)
	}
	data, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "dmon -i 7 -c 1 -s t"; got != want {
		t.Fatalf("NVIDIA dmon wrapper call=%q, want %q", got, want)
	}
}

func TestOneVMLogicalCleanupIsRunScoped(t *testing.T) {
	root := "/tmp/gpuworks-hosted-control-run-42/onevm-logical"
	owners := [2]string{"run-42-rank0", "run-42-rank1"}
	command := oneVMLogicalCleanupCommand(root, owners)
	for _, required := range []string{
		root, "/rank0", "/rank1",
		"label=gpuworks.logical-node=$owner", owners[0], owners[1],
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("run-scoped cleanup is missing %q: %s", required, command)
		}
	}
	for _, forbidden := range []string{"pkill", "killall", "--all", "docker system prune"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("run-scoped cleanup contains broad operation %q: %s", forbidden, command)
		}
	}
}
