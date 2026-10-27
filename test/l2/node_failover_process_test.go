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
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// TestProcessALostNodesReplicaStartsOnAnotherNode proves node replacement
// across the real process boundary: a single-node replica whose agent dies
// and stays gone past the deployment's replacement wait is started on the
// other node, without spending a retry, and the move is on the timeline.
// Both agents run the recording runtime, so no container daemon is needed.
//
//	go test -tags=multiproc -run TestProcessALostNodesReplicaStartsOnAnotherNode -timeout 10m ./test/l2/
func TestProcessALostNodesReplicaStartsOnAnotherNode(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
	token := login(t, base)

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048}`), 0o644)
	_ = os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644)

	nodes := map[string]processNode{}
	agents := map[string]*exec.Cmd{}
	var modelID uint
	for _, name := range []string{"failover-a", "failover-b"} {
		seed, nodeToken := createNode(t, base, token, name)
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", filepath.Join(t.TempDir(), name))
		agent.Env = append(os.Environ(),
			"GPUWORKS_FAKE_GPU=A100x1,81920",
			"GPUWORKS_FAKE_RUNTIME_RECORD="+filepath.Join(t.TempDir(), name+".json"),
			"GPUWORKS_LOG_LEVEL=warn")
		agent.Stdout, agent.Stderr = io.Discard, io.Discard
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stopProcess(agent) })
		agents[name] = agent
		nodes[name] = waitNode(t, base, token, func(n processNode) bool { return n.UUID == seed.UUID && n.State == "online" && n.GPUCount == 1 })
		// The same model directory on both nodes, registered as one model.
		body, _ := json.Marshal(map[string]any{"node_uuid": seed.UUID, "path": modelDir, "name": "failover-model", "source_id": "failover-model"})
		resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/models/local", body)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("register local model on %s status=%d body=%s", name, resp.StatusCode, data)
		}
		var model struct {
			ID uint `json:"id"`
		}
		_ = json.Unmarshal(data, &model)
		if modelID != 0 && model.ID != modelID {
			t.Fatalf("the model registered as two models: %d and %d", modelID, model.ID)
		}
		modelID = model.ID
	}

	body, _ := json.Marshal(map[string]any{
		// Automatic placement names the node its evaluation picked; it binds
		// only the first start, the requirements stay open.
		"model_id": modelID, "placement_mode": "auto", "node_uuid": nodes["failover-a"].UUID, "served_name": "failover", "engine": "vllm",
		"image": "gpuworks/fake-engine:test", "quick_params": map[string]any{"max_model_len": 128},
	})
	resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/deployments", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d body=%s", resp.StatusCode, data)
	}
	var dep struct {
		ID              uint `json:"id"`
		NodeReplacement struct {
			AfterSec int `json:"after_sec"`
		} `json:"node_replacement"`
	}
	_ = json.Unmarshal(data, &dep)
	if dep.NodeReplacement.AfterSec != 600 {
		t.Fatalf("new deployment's replacement wait = %d, want the 600s default", dep.NodeReplacement.AfterSec)
	}
	first := waitInstance(t, base, token, func(inst processInstance) bool { return inst.DeploymentID == dep.ID && inst.ContainerID != "" })
	lost, other := "failover-a", "failover-b"
	if first.NodeUUID == nodes["failover-b"].UUID {
		lost, other = other, lost
	}

	// The shortest wait the API takes, so the case runs in about a minute.
	setBody, _ := json.Marshal(map[string]any{"after_sec": 60})
	if resp, data := authedDo(t, http.MethodPut, base, token, fmt.Sprintf("/api/v1/deployments/%d/node-replacement", dep.ID), setBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("set node replacement status=%d body=%s", resp.StatusCode, data)
	}
	stopProcess(agents[lost])
	waitNode(t, base, token, func(n processNode) bool { return n.UUID == nodes[lost].UUID && n.State == "offline" })

	deadline := time.Now().Add(3 * time.Minute)
	for {
		moved := false
		for _, inst := range listProcessInstances(t, base, token) {
			if inst.DeploymentID == dep.ID && inst.NodeUUID == nodes[other].UUID && inst.ContainerID != "" {
				moved = true
			}
		}
		if moved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the replica did not start on %s within 3 minutes of losing %s", other, lost)
		}
		time.Sleep(2 * time.Second)
	}
	resp, data = authedDo(t, http.MethodGet, base, token, fmt.Sprintf("/api/v1/deployments/%d/events?limit=200", dep.ID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status=%d body=%s", resp.StatusCode, data)
	}
	var events struct {
		Items []struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		} `json:"items"`
	}
	_ = json.Unmarshal(data, &events)
	replaced := 0
	for _, ev := range events.Items {
		if ev.Code == "NODE_REPLACED" {
			replaced++
		}
		if ev.Code == "UNIT_RETRY_EXHAUSTED" {
			t.Fatalf("the move spent the replica's retries: %+v", ev)
		}
	}
	if replaced != 1 {
		t.Fatalf("NODE_REPLACED events = %d, want 1 (%s)", replaced, data)
	}
}

// listProcessInstances reads every instance the server lists.
func listProcessInstances(t *testing.T, base, token string) []processInstance {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, base, token, "/api/v1/instances", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list instances status=%d body=%s", resp.StatusCode, data)
	}
	var instances []processInstance
	if err := apiwire.UnmarshalList(data, &instances); err != nil {
		t.Fatal(err)
	}
	return instances
}
