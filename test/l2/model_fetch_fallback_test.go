//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestModelFetchMovesPastAHangingSite is l2-model-fetch-fallback-onenode: a
// site rule offers two addresses for Hugging Face, the first of which accepts
// connections and never answers. The management node reads the model's
// metadata from the second; a real agent process downloads it from the
// second after giving up on the first, and the hanging address is never
// asked for a file.
func TestModelFetchMovesPastAHangingSite(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")

	var hung atomic.Int64
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		<-r.Context().Done()
	}))
	defer hanging.Close()
	hub := testenv.FakeModelHub(t)

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--config", loopbackOutboundConfig(t), "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(response *http.Response) bool {
		return response.StatusCode == http.StatusUnauthorized
	}, serverLogPath)
	adminToken := login(t, base)

	rule, _ := json.Marshal(map[string]any{
		"match": "https://huggingface.co", "endpoints": []string{hanging.URL, hub.Endpoint},
		"allow_upstream": false, "control_plane": true,
	})
	decodeAuthedPost(t, base, adminToken, "/api/v1/pull/source-rules", rule, http.StatusCreated, nil)

	seed, nodeToken := createNode(t, base, adminToken, "model-fetch-fallback")
	agentState := filepath.Join(t.TempDir(), "agent-state")
	agentLogPath := filepath.Join(t.TempDir(), "agent.log")
	agentLog, err := os.Create(agentLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentLog.Close() })
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", agentState)
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x1,81920",
		"GPUWORKS_FAKE_RUNTIME_RECORD="+filepath.Join(t.TempDir(), "workload.json"),
		"GPUWORKS_LOG_LEVEL=warn",
	)
	agent.Stdout, agent.Stderr = agentLog, agentLog
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 1
	})

	// The management node reads through the same rule: past the hanging
	// address to the hub, which answers with the model's config.
	registerBody, _ := json.Marshal(map[string]any{"source": "huggingface", "source_id": hub.Repo, "revision": hub.Revision, "name": "fallback-model"})
	model := struct {
		ID             uint            `json:"id"`
		Arch           string          `json:"arch"`
		MetadataNotice json.RawMessage `json:"metadata_notice"`
	}{}
	// The read waits out one probe of the hanging address first.
	request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/models", bytes.NewReader(registerBody))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || json.Unmarshal(raw, &model) != nil {
		t.Fatalf("register = %d %s", response.StatusCode, raw)
	}
	if model.Arch != "Qwen3ForCausalLM" || len(model.MetadataNotice) != 0 {
		t.Fatalf("registered model = %+v, want the metadata read from the second address", model)
	}

	downloadBody, _ := json.Marshal(map[string]string{"node_uuid": node.UUID})
	decodeAuthedPost(t, base, adminToken, fmt.Sprintf("/api/v1/models/%d/download", model.ID), downloadBody, http.StatusAccepted, nil)
	// The agent gives the hanging address one listing (bounded at 30 s)
	// before it moves on.
	presence := waitModelPresence(t, base, adminToken, model.ID, node.UUID, 2*time.Minute)
	if presence.State != "present" {
		agentOutput, _ := os.ReadFile(agentLogPath)
		t.Fatalf("model presence = %+v\n%s", presence, agentOutput)
	}
	if hung.Load() == 0 {
		t.Fatal("the hanging address was never tried: the rule's order was not followed")
	}
	downloaded := filepath.Join(agentState, "models", "huggingface", strings.ReplaceAll(hub.Repo, "/", "_")+"-"+hub.Revision, "config.json")
	if body, err := os.ReadFile(downloaded); err != nil || !strings.Contains(string(body), "Qwen3ForCausalLM") {
		t.Fatalf("downloaded config = %q (err %v)", body, err)
	}
}
