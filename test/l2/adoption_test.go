//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestProcessOutOfBandRecoveryAdoptsDockerContainer exercises the part of the
// multiprocess pyramid that an in-process fake cannot prove: a real agent
// process dies while Docker keeps the named inference container alive, then
// server recovery reaches the node over real SSH, restarts a fresh process,
// and the agent adopts that container by workload name. It is skipped when
// Docker is unavailable (the ordinary L2 reconnect test remains daemon-free).
func TestProcessOutOfBandRecoveryAdoptsDockerContainer(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker required in strict mode: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker daemon unavailable in strict mode: %v", err)
		}
		t.Skipf("docker daemon unavailable: %v", err)
	}
	root := repoRoot()
	if err := ensureFakeEngineImage(t, root); err != nil {
		t.Fatal(err)
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	startServer := func() *exec.Cmd {
		server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir,
			"--recovery-grace-period", "500ms", "--recovery-initial-interval", "200ms",
			"--recovery-max-interval", "2s", "--recovery-restart-wait", "10s")
		server.Stdout, server.Stderr = serverLog, serverLog
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		return server
	}
	server := startServer()
	t.Cleanup(func() {
		stopProcess(server)
	})
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "adoption-node")
	state := filepath.Join(t.TempDir(), "agent-state")
	var agentsMu sync.Mutex
	var agents []*exec.Cmd
	startAgent := func() (*exec.Cmd, error) {
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", state)
		agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_FAKE_GPU_NO_INJECT=1", "GPUWORKS_RUNTIME_DRIVER=docker", "GPUWORKS_LOG_LEVEL=warn")
		agent.Stdout, agent.Stderr = io.Discard, io.Discard
		if err := agent.Start(); err != nil {
			return nil, err
		}
		agentsMu.Lock()
		agents = append(agents, agent)
		agentsMu.Unlock()
		return agent, nil
	}
	agent, err := startAgent()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agentsMu.Lock()
		started := append([]*exec.Cmd(nil), agents...)
		agentsMu.Unlock()
		for _, process := range started {
			stopProcess(process)
		}
	})
	node := waitNode(t, base, adminToken, func(n processNode) bool { return n.State == "online" && n.GPUCount == 1 })
	if node.UUID != seed.UUID {
		t.Fatalf("adoption node identity changed: got=%+v seed=%+v", node, seed)
	}
	var probeCommands, restartCommands atomic.Int32
	sshHost, sshPort := startSSHCommandFixture(t, "recovery", "recovery-password", func(command string) (string, string, uint32) {
		switch {
		case command == "id -u":
			return "0\n", "", 0
		case strings.Contains(command, "test -x") && strings.Contains(command, "echo installed"):
			return "installed\n", "", 0
		case strings.Contains(command, "systemctl is-active") && strings.Contains(command, "echo not-running"):
			probeCommands.Add(1)
			return "not-running\n", "", 0
		case strings.Contains(command, "systemctl cat gpuworks-agent"):
			return "", "", 0
		case strings.Contains(command, "systemctl show gpuworks-agent -p ExecStart"):
			// The unit's --server, which restart checks before restarting it.
			return base + "\n", "", 0
		case strings.Contains(command, "systemctl restart gpuworks-agent"):
			restartCommands.Add(1)
			if _, err := startAgent(); err != nil {
				return "", err.Error(), 1
			}
			return "", "", 0
		default:
			return "", "unexpected fixture command: " + command, 1
		}
	})
	credentialBody, _ := json.Marshal(map[string]any{
		"host": sshHost, "port": sshPort, "user": "recovery", "password": "recovery-password",
	})
	credentialReq, err := http.NewRequest(http.MethodPut, base+"/api/v1/nodes/"+node.UUID+"/ssh", strings.NewReader(string(credentialBody)))
	if err != nil {
		t.Fatal(err)
	}
	credentialReq.Header.Set("Authorization", "Bearer "+adminToken)
	credentialReq.Header.Set("Content-Type", "application/json")
	credentialResp, err := processHTTPClient.Do(credentialReq)
	if err != nil {
		t.Fatal(err)
	}
	credentialResponseBody, _ := io.ReadAll(credentialResp.Body)
	credentialResp.Body.Close()
	if credentialResp.StatusCode != http.StatusOK {
		t.Fatalf("save recovery credential status=%d body=%s", credentialResp.StatusCode, credentialResponseBody)
	}
	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "adopt-model"})
	localResp, err := authedPost(t, base, adminToken, "/api/v1/models/local", localBody)
	if err != nil || localResp.StatusCode != http.StatusCreated {
		var data []byte
		status := "<nil>"
		if localResp != nil {
			data, _ = io.ReadAll(localResp.Body)
			localResp.Body.Close()
			status = fmt.Sprint(localResp.StatusCode)
		}
		t.Fatalf("register local model status=%s body=%s err=%v", status, data, err)
	}
	var model struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(localResp.Body).Decode(&model)
	localResp.Body.Close()
	deployBody, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": node.UUID, "served_name": "adopt-model", "engine": "vllm", "image": "gpuworks/fake-engine:test", "quick_params": map[string]any{"max_model_len": 128}})
	depResp, err := authedPost(t, base, adminToken, "/api/v1/deployments", deployBody)
	if err != nil || depResp.StatusCode != http.StatusCreated {
		var data []byte
		status := "<nil>"
		if depResp != nil {
			data, _ = io.ReadAll(depResp.Body)
			depResp.Body.Close()
			status = fmt.Sprint(depResp.StatusCode)
		}
		t.Fatalf("create deployment status=%s body=%s err=%v", status, data, err)
	}
	depResp.Body.Close()
	instance := waitInstance(t, base, adminToken, func(inst processInstance) bool {
		return inst.NodeUUID == node.UUID && inst.State == "running" && inst.ContainerID != ""
	})
	containerRef := instance.ContainerID
	containerOut, err := exec.Command("docker", "inspect", containerRef, "--format", "{{.Id}}").Output()
	if err != nil || strings.TrimSpace(string(containerOut)) == "" {
		t.Fatalf("running deployment container %q is not inspectable: %v %s", containerRef, err, containerOut)
	}
	oldContainerID := strings.TrimSpace(string(containerOut))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", oldContainerID).Run() })
	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = agent.Process.Wait()
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == node.UUID && n.State == "offline" })
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == node.UUID && n.State == "online" })
	if probeCommands.Load() == 0 || restartCommands.Load() != 1 {
		t.Fatalf("automatic recovery commands = probes=%d restarts=%d", probeCommands.Load(), restartCommands.Load())
	}
	reconnected := waitInstance(t, base, adminToken, func(inst processInstance) bool {
		return inst.UUID == instance.UUID && inst.NodeUUID == node.UUID && inst.State == "running" && inst.ContainerID != ""
	})
	// ContainerID may be reported as the canonical Docker ID before reconnect
	// and as the stable gpuworks-* name after adoption. Resolve both forms via
	// Docker and compare the canonical ID instead of their representation.
	afterOut, err := exec.Command("docker", "inspect", reconnected.ContainerID, "--format", "{{.Id}} {{.State.Running}}").Output()
	if err != nil {
		t.Fatalf("reconnected container %q is not inspectable: %v", reconnected.ContainerID, err)
	}
	afterFields := strings.Fields(string(afterOut))
	if len(afterFields) != 2 || afterFields[0] != oldContainerID || afterFields[1] != "true" {
		t.Fatalf("agent restart did not adopt the exact existing container: before=%q after=%v", oldContainerID, afterFields)
	}
}

func ensureFakeEngineImage(t *testing.T, root string) error {
	t.Helper()
	build := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(root, "test/fake-engine/fake-engine"), "./test/fake-engine")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build fake engine: %v: %s", err, out)
	}
	engineDir := filepath.Join(root, "test/fake-engine")
	for _, alias := range []string{"vllm", "sglang"} {
		path := filepath.Join(engineDir, alias)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("prepare fake engine %s alias: %w", alias, err)
		}
		if err := os.Link(filepath.Join(engineDir, "fake-engine"), path); err != nil {
			return fmt.Errorf("link fake engine %s alias: %w", alias, err)
		}
	}
	if out, err := exec.Command("tar", "-cf", filepath.Join(engineDir, "aliases.tar"), "-C", engineDir, "fake-engine", "vllm", "sglang").CombinedOutput(); err != nil {
		return fmt.Errorf("archive fake engine aliases: %v: %s", err, out)
	}
	if out, err := exec.Command("docker", "build", "-t", "gpuworks/fake-engine:test", filepath.Join(root, "test/fake-engine")).CombinedOutput(); err != nil {
		return fmt.Errorf("build fake engine image: %v: %s", err, out)
	}
	return nil
}
