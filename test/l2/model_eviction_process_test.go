//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// TestModelEvictionSurvivesAgentReconnect crosses real server/agent process
// boundaries. It proves eviction removes the exact node-local tree and, more
// importantly, that deleting the model_files row prevents a reconnecting agent
// from receiving a stale "present" desired state and recreating it.
func TestModelEvictionSurvivesAgentReconnect(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "model-eviction-process")
	stateDir := filepath.Join(t.TempDir(), "agent-state")
	agentLogPath := filepath.Join(t.TempDir(), "agent.log")
	agentLog, err := os.Create(agentLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentLog.Close() })
	startAgent := func() *exec.Cmd {
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", stateDir)
		agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_FAKE_GPU_NO_INJECT=1", "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_LOG_LEVEL=warn")
		agent.Stdout, agent.Stderr = agentLog, agentLog
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		return agent
	}
	agent := startAgent()
	t.Cleanup(func() {
		if agent != nil {
			stopProcess(agent)
		}
	})
	node := waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == seed.UUID && n.State == "online" })

	modelDir := filepath.Join(stateDir, "models", "fixture", "evict-me")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	registerBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "evict-process"})
	registerResp, err := authedPost(t, base, adminToken, "/api/v1/models/local", registerBody)
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		ID uint `json:"id"`
	}
	registerResponseBody, _ := io.ReadAll(registerResp.Body)
	registerResp.Body.Close()
	if registerResp.StatusCode != http.StatusCreated || json.Unmarshal(registerResponseBody, &model) != nil || model.ID == 0 {
		t.Fatalf("register local model status=%d body=%s", registerResp.StatusCode, registerResponseBody)
	}

	evictReq, err := http.NewRequest(http.MethodDelete, base+"/api/v1/models/"+fmt.Sprint(model.ID)+"/nodes/"+node.UUID, nil)
	if err != nil {
		t.Fatal(err)
	}
	evictReq.Header.Set("Authorization", "Bearer "+adminToken)
	evictResp, err := processHTTPClient.Do(evictReq)
	if err != nil {
		t.Fatal(err)
	}
	evictBody, _ := io.ReadAll(evictResp.Body)
	evictResp.Body.Close()
	if evictResp.StatusCode != http.StatusOK {
		t.Fatalf("evict status=%d body=%s", evictResp.StatusCode, evictBody)
	}
	if _, err := os.Lstat(modelDir); !os.IsNotExist(err) {
		t.Fatalf("agent RPC did not remove model tree: %v", err)
	}

	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = agent.Process.Wait()
	agent = nil
	waitNode(t, base, adminToken, func(n processNode) bool { return n.UUID == node.UUID && n.State == "offline" })
	agent = startAgent()
	if !waitNodeUntil(t, base, adminToken, 20*time.Second, func(n processNode) bool { return n.UUID == node.UUID && n.State != "offline" }) {
		agentOutput, _ := os.ReadFile(agentLogPath)
		serverOutput, _ := os.ReadFile(serverLogPath)
		t.Fatalf("agent did not reconnect\nagent:\n%s\nserver:\n%s", agentOutput, serverOutput)
	}
	time.Sleep(750 * time.Millisecond)
	if _, err := os.Lstat(modelDir); !os.IsNotExist(err) {
		t.Fatalf("evicted model tree was recreated after reconnect: %v", err)
	}
}

func waitNodeUntil(t *testing.T, base, token string, timeout time.Duration, want func(processNode) bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/nodes", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := processHTTPClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var nodes []processNode
			err := apiwire.ReadList(resp.Body, &nodes)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode /api/v1/nodes: %v", err)
			}
			for _, node := range nodes {
				if want(node) {
					return true
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
