//go:build cloudgpu && partition

package l4

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

type ascendPoolCandidate struct {
	host, jump, card, runtime string
}

type ascendPoolNode struct {
	candidate ascendPoolCandidate
	control   *testenv.Env
	uuid      string
	remoteDir string
}

// TestAscendPoolPlatformPartitionInferenceAndWholeGPU proves the product path,
// not a hand-written docker recipe. The CI fixture treats the configured hosts
// as one pool, selects the first currently usable member, attaches a temporary
// isolated agent to a real control plane, and drives local-model registration,
// partitioned deployment, chat, quota denial, stop, whole-GPU deployment and a
// second chat through public platform APIs.
func TestAscendPoolPlatformPartitionInferenceAndWholeGPU(t *testing.T) {
	candidate := selectAscendPoolCandidate(t)
	control := testenv.New(t)
	node := attachAscendPoolNode(t, control, candidate)
	waitAscendPoolCapability(t, control, node.uuid)

	modelDir := hostEnv("GPUWORKS_ASCEND_L4_MODEL", "/opt/gpuworks/models/qwen3.5-0.8b")
	modelID := registerAscendPoolModel(t, control, node.uuid, modelDir)
	image := hostEnv("GPUWORKS_ASCEND_L4_IMAGE", "172.20.9.4:10043/ai-model-registry/vllm-ascend:0.26.0rc1")

	partition := createAscendPlatformDeployment(t, control, map[string]any{
		"model_id": modelID, "node_uuid": node.uuid,
		"served_name": "qwen35-08b-ascend-platform-slice", "engine": "vllm", "image": image,
		"replicas":       2,
		"vram_limit_gib": 8, "partition_backend": accelerator.BackendAscendLogical,
		"placement":    map[string]any{"tp": 1, "gpus": []int{mustAtoi(t, candidate.card)}},
		"quick_params": map[string]any{"max_model_len": 2048, "gpu_memory_utilization": 0.8},
		"extra_args":   hostEnv("GPUWORKS_ASCEND_L4_EXTRA_ARGS", "--enforce-eager"),
	}, 2, 10*time.Minute)
	chatAscendPlatform(t, control, partition.servedName)
	for _, instance := range partition.instances {
		if instance.GPUIndexes != fmt.Sprintf("[%d]", mustAtoi(t, candidate.card)) {
			t.Fatalf("platform slice instance %s GPUs=%q, want shared physical card %s", instance.UUID, instance.GPUIndexes, candidate.card)
		}
		assertAscendPlatformQuota(t, node, instance.ContainerID)
	}
	stopAscendPlatformDeployment(t, control, partition.id)

	whole := createAscendPlatformDeployment(t, control, map[string]any{
		"model_id": modelID, "node_uuid": node.uuid,
		"served_name": "qwen35-08b-ascend-platform-whole", "engine": "vllm", "image": image,
		"placement":    map[string]any{"tp": 1, "gpus": []int{mustAtoi(t, candidate.card)}},
		"quick_params": map[string]any{"max_model_len": 2048, "gpu_memory_utilization": 0.8},
		"extra_args":   hostEnv("GPUWORKS_ASCEND_L4_EXTRA_ARGS", "--enforce-eager"),
	}, 1, 8*time.Minute)
	chatAscendPlatform(t, control, whole.servedName)

	evidence := fmt.Sprintf("pool_member=selected-at-runtime\nruntime=%s\nnode_uuid=%s\npartition_deployment=%d\npartition_replicas=%d\npartition_chat=ok\nover_quota_denied=both-replicas\nwhole_gpu_deployment=%d\nwhole_gpu_chat=ok\nplatform_api=models/local,deployments,api-keys,chat/completions\n", candidate.runtime, node.uuid, partition.id, len(partition.instances), whole.id)
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", evidence)
}

func ascendPoolCandidates() []ascendPoolCandidate {
	return []ascendPoolCandidate{
		{host: hostEnv("GPUWORKS_ASCEND_HOST", hostEnv("GPUWORKS_ASCEND_PRIMARY_HOST", "")), jump: hostEnv("GPUWORKS_ASCEND_SSH_JUMP", ""), card: hostEnv("GPUWORKS_ASCEND_L4_CARD", "3")},
		{host: hostEnv("GPUWORKS_ASCEND_HOST_2", ""), jump: hostEnv("GPUWORKS_ASCEND_SSH_JUMP_2", ""), card: hostEnv("GPUWORKS_ASCEND_L4_CARD_2", hostEnv("GPUWORKS_ASCEND_L4_CARD", "3"))},
	}
}

func selectAscendPoolCandidate(t *testing.T) ascendPoolCandidate {
	t.Helper()
	image := hostEnv("GPUWORKS_ASCEND_L4_IMAGE", "172.20.9.4:10043/ai-model-registry/vllm-ascend:0.26.0rc1")
	model := hostEnv("GPUWORKS_ASCEND_L4_MODEL", "/opt/gpuworks/models/qwen3.5-0.8b")
	var diagnostics []string
	for _, candidate := range ascendPoolCandidates() {
		if candidate.host == "" {
			continue
		}
		// Every check reports which condition failed on stderr: a bare
		// `set -eu` script exits silently and the CI log only shows the login
		// banner plus "exit status 1", which is what hid the September
		// fixture/Image degradation for days.
		command := fmt.Sprintf(`fail() { echo "ascend-pool preflight: $1" >&2; exit 1; }
test "$(uname -m)" = aarch64 || fail "arch is $(uname -m), want aarch64"
command -v npu-smi >/dev/null || fail "npu-smi missing"
command -v gzip >/dev/null || fail "gzip missing"
command -v curl >/dev/null || fail "curl missing"
test -f %s/config.json || fail "model fixture missing at %s"
info=$(npu-smi info) || fail "npu-smi info failed"
%s
test -n "$selected" || fail "no idle NPU: preferred %s and every other card either runs a process or holds more than %d MB of HBM (container/vNPU tenants hold HBM without listing a process)"
echo card="$selected"
if command -v docker >/dev/null && docker info >/dev/null 2>&1 && docker image inspect %s >/dev/null 2>&1; then
  echo runtime=docker
elif command -v nerdctl >/dev/null && nerdctl info >/dev/null 2>&1 && nerdctl -n gpuworks image inspect %s >/dev/null 2>&1; then
  echo runtime=nerdctl
else
  fail "acceptance image %s not present in docker, or in nerdctl's gpuworks namespace (the agent runs containers there)"
fi`, shellQuoteL4(model), shellQuoteL4(model), ascendIdleCardSelection(candidate.card), candidate.card, ascendPoolMaxIdleHBMMB(), shellQuoteL4(image), shellQuoteL4(image), shellQuoteL4(image))
		if os.Getenv("GPUWORKS_ASCEND_L4_RUNTIME") == "udocker" {
			boundary := strings.Index(command, "if command -v docker")
			command = command[:boundary] + "test -d " + shellQuoteL4(os.Getenv("GPUWORKS_ASCEND_L4_RUNVEIL_CACHE")+"/repos") + " || fail 'runveil image fixture missing'\necho runtime=udocker\n"
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		out, err := partitionSSHCommand(ctx, candidate.host, candidate.jump, command).CombinedOutput()
		cancel()
		if err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("%s unavailable: %v: %s", candidate.host, err, strings.TrimSpace(string(out))))
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, "runtime="); ok {
				candidate.runtime = value
			}
			if value, ok := strings.CutPrefix(line, "card="); ok {
				candidate.card = value
			}
		}
		if candidate.runtime == "docker" || candidate.runtime == "nerdctl" || candidate.runtime == "udocker" {
			t.Logf("Ascend pool selected %s (%s, card %s); other members remain eligible for later tests", candidate.host, candidate.runtime, candidate.card)
			return candidate
		}
	}
	message := "no Ascend pool member currently has SSH, an idle configured card, model fixture, and application image"
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("%s:\n%s", message, strings.Join(diagnostics, "\n"))
	}
	skipOrFailExternal(t, "%s: %s", message, strings.Join(diagnostics, "; "))
	return ascendPoolCandidate{}
}

func attachAscendPoolNode(t *testing.T, control *testenv.Env, candidate ascendPoolCandidate) *ascendPoolNode {
	t.Helper()
	api := control.API(t)
	remotePort := reserveAscendRemotePort(t, candidate)
	body, _ := json.Marshal(map[string]any{
		"name": "Ascend CI pool temporary node", "connect_mode": "server_dial",
		"server_dial_listen": "0.0.0.0:" + remotePort,
		"host_address":       ascendPoolHostAddress(candidate.host), "server_url": control.URL(),
	})
	resp, err := api.PostJSON("/api/v1/nodes", body)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create temporary pool node status=%d body=%s", resp.StatusCode, data)
	}
	var enrolled struct {
		Node  dto.Node `json:"node"`
		Token string   `json:"token"`
	}
	if err := json.Unmarshal(data, &enrolled); err != nil || enrolled.Node.UUID == "" || enrolled.Token == "" {
		t.Fatalf("decode temporary pool node: %v body=%s", err, data)
	}

	remoteDir := "/tmp/gpuworks-ascend-pool-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	prepareCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
	out, err := partitionSSHCommand(prepareCtx, candidate.host, candidate.jump, "mkdir "+shellQuoteL4(remoteDir)).CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("prepare isolated pool agent: %v: %s", err, out)
	}
	node := &ascendPoolNode{candidate: candidate, control: control, uuid: enrolled.Node.UUID, remoteDir: remoteDir}
	t.Cleanup(func() { node.cleanup(t) })

	agent, readErr := agentbin.Read("", "gpuworks-agent-linux-arm64")
	if path := os.Getenv("GPUWORKS_ASCEND_L4_AGENT"); path != "" {
		agent, readErr = os.ReadFile(path)
	}
	if readErr != nil || len(agent) < 1<<20 {
		t.Fatalf("self-contained arm64 agent asset is missing; run make cross-agents: size=%d err=%v", len(agent), readErr)
	}
	var compressed bytes.Buffer
	zw, compressErr := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if compressErr != nil {
		t.Fatal(compressErr)
	}
	if _, compressErr := zw.Write(agent); compressErr != nil {
		t.Fatal(compressErr)
	}
	if compressErr := zw.Close(); compressErr != nil {
		t.Fatal(compressErr)
	}
	if out, err = downloadAscendPoolAgent(t, candidate, remoteDir, compressed.Bytes()); err != nil {
		t.Fatalf("transfer self-contained compressed pool agent: %v: %s", err, out)
	}
	if candidate.runtime == "udocker" {
		stageAscendRunveilCache(t, node)
	}
	start := fmt.Sprintf("chmod +x %s/gpuworks-agent; nohup %s/gpuworks-agent run --server %s --token %s --state %s/state --driver %s --connect-mode server_dial --listen %s >%s/agent.stdout 2>&1 & echo $! >%s/agent.pid", shellQuoteL4(remoteDir), shellQuoteL4(remoteDir), shellQuoteL4(control.URL()), shellQuoteL4(enrolled.Token), shellQuoteL4(remoteDir), shellQuoteL4(candidate.runtime), shellQuoteL4("0.0.0.0:"+remotePort), shellQuoteL4(remoteDir), shellQuoteL4(remoteDir))
	startCtx, startCancel := context.WithTimeout(t.Context(), time.Minute)
	out, err = partitionSSHCommand(startCtx, candidate.host, candidate.jump, start).CombinedOutput()
	startCancel()
	if err != nil {
		t.Fatalf("start isolated pool agent: %v: %s", err, out)
	}

	ctx, dialCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer dialCancel()
	dialAddr := net.JoinHostPort(ascendPoolHostAddress(candidate.host), remotePort)
	for {
		err = control.DialServerAgent(ctx, dialAddr, enrolled.Token)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("dial isolated Ascend pool agent at %s: %v", dialAddr, err)
		case <-time.After(time.Second):
		}
	}
	control.WaitNodeOnlineUUID(t, enrolled.Node.UUID)
	return node
}

func downloadAscendPoolAgent(t *testing.T, candidate ascendPoolCandidate, remoteDir string, compressed []byte) ([]byte, error) {
	t.Helper()
	remoteHost := ascendPoolHostAddress(candidate.host)
	route, err := net.DialTimeout("udp", net.JoinHostPort(remoteHost, "22"), 5*time.Second)
	if err == nil {
		localIP := route.LocalAddr().(*net.UDPAddr).IP.String()
		_ = route.Close()
		listener, listenErr := net.Listen("tcp", "0.0.0.0:0")
		if listenErr == nil {
			path := "/gpuworks-agent-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".gz"
			mux := http.NewServeMux()
			mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
				_, _ = w.Write(compressed)
			})
			server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go func() { _ = server.Serve(listener) }()
			port := listener.Addr().(*net.TCPAddr).Port
			url := "http://" + net.JoinHostPort(localIP, strconv.Itoa(port)) + path
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			command := "curl -fsS " + shellQuoteL4(url) + " | gzip -d > " + shellQuoteL4(remoteDir+"/gpuworks-agent")
			out, transferErr := partitionSSHCommand(ctx, candidate.host, candidate.jump, command).CombinedOutput()
			cancel()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = server.Shutdown(closeCtx)
			closeCancel()
			if transferErr == nil {
				return out, nil
			}
			t.Logf("private HTTP agent transfer unavailable (%v: %s); falling back to compressed SSH stream", transferErr, strings.TrimSpace(string(out)))
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := "cat > " + shellQuoteL4(remoteDir+"/gpuworks-agent.gz") + " && gzip -df " + shellQuoteL4(remoteDir+"/gpuworks-agent.gz")
	cmd := partitionSSHCommand(ctx, candidate.host, candidate.jump, command)
	cmd.Stdin = bytes.NewReader(compressed)
	return cmd.CombinedOutput()
}

func reserveAscendRemotePort(t *testing.T, candidate ascendPoolCandidate) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := `python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'`
	out, err := partitionSSHCommand(ctx, candidate.host, candidate.jump, command).CombinedOutput()
	if err != nil {
		t.Fatalf("reserve isolated agent port: %v: %s", err, out)
	}
	port := ""
	for _, line := range strings.Split(string(out), "\n") {
		candidate := strings.TrimSpace(line)
		if _, parseErr := strconv.Atoi(candidate); parseErr == nil {
			port = candidate
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("invalid isolated agent port %q", port)
	}
	return port
}

func ascendPoolHostAddress(target string) string {
	if _, host, ok := strings.Cut(target, "@"); ok {
		return host
	}
	return target
}

func (n *ascendPoolNode) cleanup(t *testing.T) {
	t.Helper()
	if t.Failed() && os.Getenv("GPUWORKS_KEEP_ON_FAILURE") == "1" {
		t.Logf("preserving failed Ascend test state at %s; scoped cleanup required", n.remoteDir)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := "test ! -f " + shellQuoteL4(n.remoteDir+"/agent.pid") + " || kill $(cat " + shellQuoteL4(n.remoteDir+"/agent.pid") + ")"
		if out, err := partitionSSHCommand(ctx, n.candidate.host, n.candidate.jump, command).CombinedOutput(); err != nil {
			t.Logf("freeze failed agent: %v %s", err, out)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := fmt.Sprintf(`set +e
dir=%s
if test -f "$dir/agent.pid"; then kill "$(cat "$dir/agent.pid")" 2>/dev/null || true; fi
case "$dir" in /tmp/gpuworks-ascend-pool-*) rm -rf -- "$dir" ;; *) exit 2 ;; esac`, shellQuoteL4(n.remoteDir))
	if out, err := partitionSSHCommand(ctx, n.candidate.host, n.candidate.jump, command).CombinedOutput(); err != nil {
		t.Errorf("cleanup isolated Ascend pool agent: %v: %s", err, out)
	}
}

func waitAscendPoolCapability(t *testing.T, control *testenv.Env, uuid string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		node, err := control.Store().NodeWithGPUs(uuid)
		if err == nil && len(node.GPUs) > 0 {
			for _, capability := range partitioncap.ForGPU(node, &node.GPUs[0]) {
				if capability.Backend == accelerator.BackendAscendLogical && capability.Status == partitioncap.StatusReady {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	node, _ := control.Store().NodeWithGPUs(uuid)
	t.Fatalf("temporary Ascend pool node did not report a ready real partition capability: %+v", node)
}

func registerAscendPoolModel(t *testing.T, control *testenv.Env, nodeUUID, path string) uint {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"node_uuid": nodeUUID, "path": path, "name": "Qwen3.5-0.8B Ascend pool"})
	resp, err := control.API(t).PostJSON("/api/v1/models/local", payload)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("platform local-model registration status=%d body=%s", resp.StatusCode, data)
	}
	var model dto.Model
	if err := json.Unmarshal(data, &model); err != nil || model.ID == 0 {
		t.Fatalf("decode platform local model: %v body=%s", err, data)
	}
	return model.ID
}

type ascendPlatformDeployment struct {
	id         uint
	servedName string
	instances  []store.Instance
}

func createAscendPlatformDeployment(t *testing.T, control *testenv.Env, body map[string]any, expected int, timeout time.Duration) ascendPlatformDeployment {
	t.Helper()
	payload, _ := json.Marshal(body)
	resp, err := control.API(t).PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("platform deployment create status=%d body=%s", resp.StatusCode, data)
	}
	var created dto.Deployment
	if err := json.Unmarshal(data, &created); err != nil || created.ID == 0 {
		t.Fatalf("decode platform deployment: %v body=%s", err, data)
	}
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("GPUWORKS_KEEP_ON_FAILURE") == "1" {
			return
		}
		stopAscendPlatformDeployment(t, control, created.ID)
	})
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		instances, _ := control.Store().InstancesForDeployment(created.ID)
		var running []store.Instance
		for _, instance := range instances {
			if instance.State == proto.InstRunning && instance.ContainerID != "" {
				running = append(running, instance)
			}
		}
		if len(running) == expected {
			return ascendPlatformDeployment{id: created.ID, servedName: fmt.Sprint(body["served_name"]), instances: running}
		}
		time.Sleep(2 * time.Second)
	}
	instances, _ := control.Store().InstancesForDeployment(created.ID)
	t.Fatalf("platform deployment %d did not become running within %s: %+v", created.ID, timeout, instances)
	return ascendPlatformDeployment{}
}

func chatAscendPlatform(t *testing.T, control *testenv.Env, servedName string) {
	t.Helper()
	resp, err := control.API(t).PostJSON("/api/v1/apikeys", []byte(`{"name":"ascend-pool-l4"}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var key struct {
		Key string `json:"key"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(data, &key) != nil || key.Key == "" {
		t.Fatalf("create platform API key status=%d body=%s", resp.StatusCode, data)
	}
	chatBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"只回答：成功"}],"max_tokens":16,"temperature":0}`, servedName)
	req, _ := http.NewRequest(http.MethodPost, control.URL()+"/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Content-Type", "application/json")
	chatResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer chatResp.Body.Close()
	data, _ = io.ReadAll(chatResp.Body)
	if chatResp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"choices"`)) {
		t.Fatalf("platform chat status=%d body=%s", chatResp.StatusCode, data)
	}
}

func assertAscendPlatformQuota(t *testing.T, node *ascendPoolNode, containerID string) {
	t.Helper()
	if node.candidate.runtime == "udocker" {
		assertAscendRunveilQuota(t, node, containerID)
		return
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`).MatchString(containerID) {
		t.Fatalf("unsafe runtime container id %q", containerID)
	}
	// Docker has no namespaces, but the agent pins every nerdctl invocation
	// to the gpuworks containerd namespace — an unpinned `nerdctl exec` looks
	// in the default namespace (k8s.io on these hosts) and misses the
	// platform-created containers.
	cli := node.candidate.runtime
	if cli == "nerdctl" {
		cli = "nerdctl -n " + nerdctl.Namespace
	}
	command := fmt.Sprintf(`%s exec -i %s python3 - <<'PY'
import acl
acl.init()
acl.rt.set_device(0)
pointer, result = acl.rt.malloc(8 * 1024 * 1024 * 1024, 0)
if result == 0:
    acl.rt.free(pointer)
    raise SystemExit("unexpected: 8 GiB allocation succeeded while vLLM occupied the 8 GiB platform slice")
print("platform_over_quota_denied_acl", result)
acl.rt.reset_device(0)
acl.finalize()
PY`, cli, shellQuoteL4(containerID))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	out, err := partitionSSHCommand(ctx, node.candidate.host, node.candidate.jump, command).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "platform_over_quota_denied_acl") {
		t.Fatalf("verify quota on platform-created container: %v\n%s", err, out)
	}
}

func stopAscendPlatformDeployment(t *testing.T, control *testenv.Env, id uint) {
	t.Helper()
	if id == 0 {
		return
	}
	resp, err := control.API(t).PostJSON(fmt.Sprintf("/api/v1/deployments/%d/stop", id), nil)
	if err != nil {
		t.Errorf("stop platform deployment %d: %v", id, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stop platform deployment %d status=%d", id, resp.StatusCode)
		return
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		instances, _ := control.Store().InstancesForDeployment(id)
		if len(instances) == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("platform deployment %d did not stop", id)
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	n, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("invalid Ascend pool card %q", value)
	}
	return n
}
