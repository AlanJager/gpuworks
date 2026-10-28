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
)

// TestProcessAnAutomaticPDDeploymentPicksAndRepicksItsNodes proves automatic
// placement of a PD deployment across the real process boundary: the server
// picks the prefill's and the decode's nodes at create (two one-card nodes
// of four, one taken by another deployment's replica), and when the
// decode starts again after its node stopped taking replicas, it is placed
// on the free node with UNIT_PLACED on the timeline. The agents run the
// recording runtime, so no container daemon is needed.
//
//	go test -tags=multiproc -run TestProcessAnAutomaticPDDeploymentPicksAndRepicksItsNodes -timeout 10m ./test/l2/
func TestProcessAnAutomaticPDDeploymentPicksAndRepicksItsNodes(t *testing.T) {
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

	names := []string{"auto-a", "auto-b", "auto-c", "auto-d"}
	nodes := map[string]processNode{}
	byUUID := map[string]string{}
	var modelID uint
	for _, name := range names {
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
		nodes[name] = waitNode(t, base, token, func(n processNode) bool { return n.UUID == seed.UUID && n.State == "online" && n.GPUCount == 1 })
		byUUID[seed.UUID] = name
		body, _ := json.Marshal(map[string]any{"node_uuid": seed.UUID, "path": modelDir, "name": "auto-model", "source_id": "auto-model"})
		resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/models/local", body)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("register local model on %s status=%d body=%s", name, resp.StatusCode, data)
		}
		var model struct {
			ID uint `json:"id"`
		}
		_ = json.Unmarshal(data, &model)
		modelID = model.ID
	}

	// Another deployment's replica takes auto-a's only card.
	body, _ := json.Marshal(map[string]any{
		"model_id": modelID, "placement_mode": "manual", "node_uuid": nodes["auto-a"].UUID, "served_name": "occupier", "engine": "vllm",
		"image": "gpuworks/fake-engine:test", "quick_params": map[string]any{"max_model_len": 128},
	})
	if resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/deployments", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create occupier status=%d body=%s", resp.StatusCode, data)
	}
	waitInstance(t, base, token, func(inst processInstance) bool { return inst.NodeUUID == nodes["auto-a"].UUID && inst.ContainerID != "" })

	body, _ = json.Marshal(map[string]any{
		"model_id": modelID, "placement_mode": "auto", "served_name": "auto-pd", "engine": "vllm",
		"image": "gpuworks/fake-engine:test", "quick_params": map[string]any{"max_model_len": 128},
		"placement": map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{
			"prefill": map[string]any{"tp": 1, "units": []any{map[string]any{}}},
			"decode":  map[string]any{"tp": 1, "units": []any{map[string]any{}}},
		}}},
	})
	resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/deployments", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create automatic PD status=%d body=%s", resp.StatusCode, data)
	}
	var dep struct {
		ID            uint   `json:"id"`
		PlacementMode string `json:"placement_mode"`
	}
	_ = json.Unmarshal(data, &dep)
	if dep.PlacementMode != "auto" {
		t.Fatalf("placement_mode = %q, want auto", dep.PlacementMode)
	}
	roles := pdRoleNodes(t, base, token, dep.ID)
	if len(roles["prefill"]) != 1 || len(roles["decode"]) != 1 || roles["prefill"][0] == nodes["auto-a"].UUID || roles["decode"][0] == nodes["auto-a"].UUID {
		t.Fatalf("roles = %v, want both off auto-a, whose card is taken", roles)
	}
	decodeNode := roles["decode"][0]
	decode := waitInstance(t, base, token, func(inst processInstance) bool {
		return inst.DeploymentID == dep.ID && inst.NodeUUID == decodeNode && inst.ContainerID != ""
	})
	waitInstance(t, base, token, func(inst processInstance) bool {
		return inst.DeploymentID == dep.ID && inst.NodeUUID == roles["prefill"][0] && inst.ContainerID != ""
	})

	// The decode's node stops taking replicas, and the decode starts again:
	// it goes to the only node left that can take it.
	free := ""
	for _, name := range names {
		uuid := nodes[name].UUID
		if uuid != nodes["auto-a"].UUID && uuid != roles["prefill"][0] && uuid != decodeNode {
			free = uuid
		}
	}
	if free == "" {
		t.Fatalf("no free node left: prefill and decode share a node (%v)", roles)
	}
	cordon, _ := json.Marshal(map[string]any{"scheduling": "cordoned"})
	if resp, data := authedDo(t, http.MethodPut, base, token, "/api/v1/nodes/"+decodeNode+"/scheduling", cordon); resp.StatusCode != http.StatusOK {
		t.Fatalf("cordon status=%d body=%s", resp.StatusCode, data)
	}
	if resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/instances/"+decode.UUID+"/restart?drain=false", nil); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restart decode status=%d body=%s", resp.StatusCode, data)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		moved := false
		for _, inst := range listProcessInstances(t, base, token) {
			if inst.DeploymentID == dep.ID && inst.NodeUUID == free && inst.ContainerID != "" {
				moved = true
			}
		}
		if moved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the decode did not start on %s within 2 minutes (roles now %v)", byUUID[free], pdRoleNodes(t, base, token, dep.ID))
		}
		time.Sleep(2 * time.Second)
	}
	if roles := pdRoleNodes(t, base, token, dep.ID); roles["decode"][0] != free || roles["prefill"][0] == free {
		t.Fatalf("roles after the re-pick = %v, want the decode on %s and the prefill kept", roles, byUUID[free])
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
	placed := 0
	for _, ev := range events.Items {
		if ev.Code == "UNIT_PLACED" && ev.Params["role"] == "decode" {
			placed++
		}
	}
	if placed != 1 {
		t.Fatalf("UNIT_PLACED for the decode = %d, want 1 (%s)", placed, data)
	}
}

// pdRoleNodes are the nodes a PD deployment's units name, by role.
func pdRoleNodes(t *testing.T, base, token string, id uint) map[string][]string {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, base, token, fmt.Sprintf("/api/v1/deployments/%d", id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get deployment status=%d body=%s", resp.StatusCode, data)
	}
	var dep struct {
		PD *struct {
			Roles []struct {
				Role  string `json:"role"`
				Units []struct {
					NodeUUID string `json:"node_uuid"`
				} `json:"units"`
			} `json:"roles"`
		} `json:"pd"`
	}
	if err := json.Unmarshal(data, &dep); err != nil || dep.PD == nil {
		t.Fatalf("deployment has no pd: %v %s", err, data)
	}
	out := map[string][]string{}
	for _, role := range dep.PD.Roles {
		for _, unit := range role.Units {
			out[role.Role] = append(out[role.Role], unit.NodeUUID)
		}
	}
	return out
}
