//go:build multiproc

package l2

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type acceleratorNodeView struct {
	UUID                  string `json:"uuid"`
	State                 string `json:"state"`
	GPUVendor             string `json:"gpu_vendor"`
	GPUCount              int    `json:"gpu_count"`
	AcceleratorPreference string `json:"accelerator_preference"`
	AcceleratorVendors    []struct {
		Vendor string `json:"vendor"`
		Cards  int    `json:"cards"`
		Model  string `json:"model"`
	} `json:"accelerator_vendors"`
}

func getAcceleratorNode(t *testing.T, base, token, uuid string) acceleratorNodeView {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, base, token, "/api/v1/nodes/"+uuid, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET node status=%d body=%s", resp.StatusCode, data)
	}
	var out acceleratorNodeView
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode node: %v (%s)", err, data)
	}
	return out
}

func waitAcceleratorNode(t *testing.T, base, token, uuid, logPath string, want func(acceleratorNodeView) bool) acceleratorNodeView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last acceleratorNodeView
	for time.Now().Before(deadline) {
		last = getAcceleratorNode(t, base, token, uuid)
		if want(last) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := os.ReadFile(logPath)
	t.Fatalf("node never reached the wanted state; last %+v\nagent log:\n%s", last, logs)
	return last
}

func patchAccelerator(t *testing.T, base, token, uuid, accelerator string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"accelerator": accelerator})
	resp, data := authedDo(t, http.MethodPatch, base, token, "/api/v1/nodes/"+uuid+"/accelerator", body)
	return resp.StatusCode, string(data)
}

// A host with cards of two vendors, through real server and agent
// processes: the node manages one vendor's cards and lists both; choosing
// the other restarts the agent in place (same process, re-executed) onto
// that vendor's cards, the choice survives a restart of the agent, and a
// node with an instance on it refuses to switch.
//
//	go test -tags=multiproc -run TestAMixedHostSwitchesWhichVendorItsNodeManages -count=1 -v ./test/l2/
func TestAMixedHostSwitchesWhichVendorItsNodeManages(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "mixed-host")

	stateDir := filepath.Join(t.TempDir(), "agent-state")
	agentLogPath := filepath.Join(t.TempDir(), "agent.log")
	startAgent := func() *exec.Cmd {
		t.Helper()
		agentLog, err := os.OpenFile(agentLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = agentLog.Close() })
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", stateDir)
		agent.Env = append(os.Environ(),
			"GPUWORKS_FAKE_GPU_VENDORS=ascend:310P3x1;nvidia:RTX 4090x2",
			"GPUWORKS_FAKE_RUNTIME_RECORD="+filepath.Join(t.TempDir(), "workload.json"),
			"GPUWORKS_LOG_LEVEL=info")
		agent.Stdout, agent.Stderr = agentLog, agentLog
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stopProcess(agent) })
		return agent
	}
	agent := startAgent()

	// Automatic manages Ascend, which the agent tries before NVIDIA, and the
	// node lists both vendors.
	node := waitAcceleratorNode(t, base, adminToken, seed.UUID, agentLogPath, func(n acceleratorNodeView) bool {
		return n.State == "online" && n.GPUVendor == "ascend" && n.GPUCount == 1 && len(n.AcceleratorVendors) == 2
	})
	if node.AcceleratorVendors[0].Vendor != "ascend" || node.AcceleratorVendors[1].Vendor != "nvidia" || node.AcceleratorVendors[1].Cards != 2 || node.AcceleratorVendors[1].Model != "RTX 4090" {
		t.Fatalf("vendors = %+v", node.AcceleratorVendors)
	}
	if node.AcceleratorPreference != "" {
		t.Fatalf("preference before any choice = %q", node.AcceleratorPreference)
	}
	if code, body := patchAccelerator(t, base, adminToken, seed.UUID, "hygon"); code != http.StatusBadRequest || !strings.Contains(body, "NODE_ACCELERATOR_NOT_ON_HOST") {
		t.Fatalf("a vendor the host lacks: %d %s", code, body)
	}

	// Choosing NVIDIA re-executes the same agent process onto its cards.
	pid := agent.Process.Pid
	if code, body := patchAccelerator(t, base, adminToken, seed.UUID, "nvidia"); code != http.StatusOK || !strings.Contains(body, `"accelerator_preference":"nvidia"`) {
		t.Fatalf("switch to nvidia: %d %s", code, body)
	}
	waitAcceleratorNode(t, base, adminToken, seed.UUID, agentLogPath, func(n acceleratorNodeView) bool {
		return n.State == "online" && n.GPUVendor == "nvidia" && n.GPUCount == 2 && len(n.AcceleratorVendors) == 2
	})
	if err := agent.Process.Signal(syscall.Signal(0)); err != nil || agent.ProcessState != nil {
		t.Fatalf("the agent process %d is gone after the switch: %v", pid, err)
	}
	// The agent's own log lives in its state directory; stderr only gets
	// the warnings.
	logs, _ := os.ReadFile(filepath.Join(stateDir, "agent.log"))
	if got := strings.Count(string(logs), "agent starting"); got != 2 {
		t.Fatalf("agent started %d times in process %d, want 2 (once, then re-executed):\n%s", got, pid, logs)
	}
	if !strings.Contains(string(logs), "restarting to manage another accelerator") {
		t.Fatalf("the restart was not logged:\n%s", logs)
	}
	if raw, err := os.ReadFile(filepath.Join(stateDir, "accelerator-preference")); err != nil || strings.TrimSpace(string(raw)) != "nvidia" {
		t.Fatalf("stored choice = %q, %v", raw, err)
	}

	// A fresh agent process reads the choice from its state directory.
	stopProcess(agent)
	waitAcceleratorNode(t, base, adminToken, seed.UUID, agentLogPath, func(n acceleratorNodeView) bool { return n.State != "online" })
	agent = startAgent()
	waitAcceleratorNode(t, base, adminToken, seed.UUID, agentLogPath, func(n acceleratorNodeView) bool {
		return n.State == "online" && n.GPUVendor == "nvidia" && n.GPUCount == 2
	})
	logs, _ = os.ReadFile(filepath.Join(stateDir, "agent.log"))
	if !strings.Contains(string(logs), "managing the chosen accelerator") {
		t.Fatalf("the new process did not start on the stored choice:\n%s", logs)
	}

	// An instance on the node blocks the switch.
	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": seed.UUID, "path": modelDir, "name": "mixed-host-model"})
	localResp, localData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models/local", localBody)
	if localResp.StatusCode != http.StatusCreated {
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, localData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(localData, &model); err != nil {
		t.Fatal(err)
	}
	depBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": seed.UUID, "name": "mixed-host", "served_name": "mixed-host",
		"engine": "vllm", "image": "gpuworks/fake-engine:test",
		"quick_params": map[string]any{"max_model_len": 1024},
		"placement":    map[string]any{"tp": 1},
	})
	depResp, depData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", depBody)
	if depResp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d body=%s", depResp.StatusCode, depData)
	}
	var dep l2Deployment
	if err := json.Unmarshal(depData, &dep); err != nil {
		t.Fatal(err)
	}
	waitLaunchConfigInstance(t, base, adminToken, dep.ID)
	if code, body := patchAccelerator(t, base, adminToken, seed.UUID, "auto"); code != http.StatusConflict || !strings.Contains(body, "NODE_ACCELERATOR_HAS_INSTANCES") {
		t.Fatalf("switch with an instance: %d %s", code, body)
	}
	if n := getAcceleratorNode(t, base, adminToken, seed.UUID); n.AcceleratorPreference != "nvidia" || n.GPUVendor != "nvidia" {
		t.Fatalf("a refused switch changed the node: %+v", n)
	}

	// Once the deployment is gone, automatic goes back to Ascend.
	if resp, data := authedDo(t, http.MethodDelete, base, adminToken, "/api/v1/deployments/"+itoa(dep.ID), nil); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete deployment status=%d body=%s", resp.StatusCode, data)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, body := patchAccelerator(t, base, adminToken, seed.UUID, "auto")
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("switch after the deployment was deleted: %d %s", code, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitAcceleratorNode(t, base, adminToken, seed.UUID, agentLogPath, func(n acceleratorNodeView) bool {
		return n.State == "online" && n.GPUVendor == "ascend" && n.GPUCount == 1 && n.AcceleratorPreference == "auto"
	})
	if _, err := os.Stat(filepath.Join(stateDir, "accelerator-preference")); !os.IsNotExist(err) {
		t.Fatalf("automatic left the stored choice behind: %v", err)
	}
}
