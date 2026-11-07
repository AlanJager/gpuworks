//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

func authedDo(t *testing.T, method, base, token, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func authedPutJSON(t *testing.T, base, token, path string, value any) {
	t.Helper()
	body, _ := json.Marshal(value)
	resp, data := authedDo(t, http.MethodPut, base, token, path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s status=%d body=%s", path, resp.StatusCode, data)
	}
}

type l2Deployment struct {
	ID           uint   `json:"id"`
	RuntimeState string `json:"runtime_state"`
}

func getDeployment(t *testing.T, base, token string, id uint) l2Deployment {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, base, token, "/api/v1/deployments/"+itoa(id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET deployment %d status=%d body=%s", id, resp.StatusCode, data)
	}
	var out l2Deployment
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode deployment: %v (%s)", err, data)
	}
	return out
}

func itoa(v uint) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// A deployment the scheduler cannot place used to sit in `pending` forever with
// no reason reachable from the API: the scheduler's Reason was computed and
// dropped into a log line, no event was published, and because the work queue
// is purely event-driven nothing would ever reconcile it again. This drives the
// whole thing through real server and agent processes.
func TestUnschedulableDeploymentBecomesDiagnosable(t *testing.T) {
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

	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "unschedulable-process")

	agentState := filepath.Join(t.TempDir(), "agent-state")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", agentState)
	agent.Env = append(os.Environ(),
		// One GPU: a request for two can never be placed, one for one can
		// once the node is back.
		"GPUWORKS_FAKE_GPU=A100x1,81920",
		"GPUWORKS_LOG_LEVEL=warn",
	)
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 1
	})

	// The floor is small precisely so this test does not have to wait a minute
	// of wall clock to observe the transition.
	authedPutJSON(t, base, adminToken, "/api/v1/settings/deployment", map[string]any{
		"pending_timeout_sec":   5,
		"event_retention_days":  180,
		"event_retention_bytes": int64(50) << 30,
	})

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Registering a local path makes the model file present immediately, so the
	// controller reaches the scheduler instead of waiting on a download.
	localBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "unschedulable-model"})
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

	deploy := func(tp int) (*http.Response, []byte) {
		body, _ := json.Marshal(map[string]any{
			"model_id": model.ID, "node_uuid": node.UUID, "served_name": "unschedulable",
			"engine": "vllm", "image": "gpuworks/fake-engine:test",
			"quick_params": map[string]any{"max_model_len": 128},
			"placement":    map[string]any{"tp": tp},
		})
		return authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", body)
	}
	// Two GPUs on a one-GPU node can never be placed, however long it waits:
	// the request is refused at once rather than stored to wait forever.
	if resp, data := deploy(2); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "DEPLOYMENT_NEVER_FITS") {
		t.Fatalf("never-fitting create status=%d body=%s", resp.StatusCode, data)
	}
	// One GPU fits the node, which is offline: that only has to wait, so the
	// deployment is created and the wait must be diagnosable.
	stopProcess(agent)
	waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State != "online"
	})
	depResp, depData := deploy(1)
	if depResp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d body=%s", depResp.StatusCode, depData)
	}
	var dep l2Deployment
	if err := json.Unmarshal(depData, &dep); err != nil {
		t.Fatal(err)
	}

	// The reason has to exist before the timeout does. Poll until the scheduler
	// has recorded its refusal, so the test proves the transition rather than
	// racing it.
	var reasonCode string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, evData := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/deployments/"+itoa(dep.ID)+"/events", nil)
		var events []struct {
			Kind   string         `json:"kind"`
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := apiwire.UnmarshalCursorPage(evData, &events); err != nil {
			t.Fatalf("decode deployment events: %v (%s)", err, evData)
		}
		for _, ev := range events {
			if ev.Kind == "schedule" {
				scheduler, _ := ev.Params["scheduler"].(map[string]any)
				if code, _ := scheduler["code"].(string); code != "" {
					reasonCode = code
				}
			}
		}
		if reasonCode != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if reasonCode == "" {
		t.Fatalf("the scheduler's refusal never became readable; a deployment that cannot be placed is undiagnosable again")
	}
	if reasonCode != "pool_empty" {
		t.Fatalf("scheduler reason = %q, want pool_empty: the only candidate is offline", reasonCode)
	}

	// A refusal alone must NOT read as a failure - the deployment is still
	// within its window and could yet be placed.
	if got := getDeployment(t, base, adminToken, dep.ID).RuntimeState; got != "pending" {
		t.Fatalf("runtime_state inside the window = %q, want pending", got)
	}

	// Past the window the deployment must stop claiming to be merely queued.
	// This also exercises the self-scheduled retry: nothing else touches the
	// deployment, so only the re-enqueue can produce this transition.
	waitDeploymentState(t, base, adminToken, dep.ID, "failed", 30*time.Second)

	// The reason must survive the deployment being deleted: that is the case an
	// operator is actually in when they go looking for it.
	if resp, data := authedDo(t, http.MethodDelete, base, adminToken, "/api/v1/deployments/"+itoa(dep.ID), nil); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete deployment status=%d body=%s", resp.StatusCode, data)
	}
	_, afterData := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/deployments/"+itoa(dep.ID)+"/events", nil)
	var after []struct {
		Kind string `json:"kind"`
	}
	if err := apiwire.UnmarshalCursorPage(afterData, &after); err != nil {
		t.Fatalf("events unreadable after delete: %v (%s)", err, afterData)
	}
	if len(after) == 0 {
		t.Fatalf("deleting the deployment erased the reason it failed")
	}
}

func waitDeploymentState(t *testing.T, base, token string, id uint, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = getDeployment(t, base, token, id).RuntimeState
		if last == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Dump the whole deployment so a failure shows WHY, not just that it failed.
	_, detail := authedDo(t, http.MethodGet, base, token, "/api/v1/deployments/"+itoa(id), nil)
	t.Fatalf("deployment %d never reached %q (last=%q) detail=%s", id, want, last, detail)
}

// Deleting a deployment must not drop the record before the node has confirmed
// the teardown, because that record is the last handle on a container that may
// still be running with its GPU allocated. Force is the explicit exception: it
// guarantees the control-plane record goes away even when the node is gone.
func TestDeleteWaitsForTheAgentUnlessForced(t *testing.T) {
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

	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "delete-process")

	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken,
		"--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x1,81920",
		// The recording runtime is the environment-selected fake for exactly
		// this boundary: it reports every image as present and records the
		// workload instead of asking a container runtime to run it.
		"GPUWORKS_FAKE_RUNTIME_RECORD="+filepath.Join(t.TempDir(), "workload.json"),
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 1
	})

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048}`), 0o644)
	_ = os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644)
	localBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "delete-model"})
	localResp, localData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models/local", localBody)
	if localResp.StatusCode != http.StatusCreated {
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, localData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	_ = json.Unmarshal(localData, &model)

	// "Started" rather than "running": the recording runtime accepts the
	// workload and reports a container id, but nothing serves health behind it,
	// so `running` is unreachable here by construction. The delete semantics do
	// not depend on health - what matters is that a container exists to be
	// reaped.
	createStarted := func(name string) l2Deployment {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model_id": model.ID, "node_uuid": node.UUID, "served_name": name,
			"engine": "vllm", "image": "gpuworks/fake-engine:test",
			"quick_params": map[string]any{"max_model_len": 128},
		})
		resp, data := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create deployment status=%d body=%s", resp.StatusCode, data)
		}
		var dep l2Deployment
		_ = json.Unmarshal(data, &dep)
		waitDeploymentState(t, base, adminToken, dep.ID, "starting", 60*time.Second)
		return dep
	}

	// An ordinary delete is asynchronous: it asks, and waits for the agent to
	// confirm. The row surviving the first moments is the point — dropping it
	// immediately is what used to orphan a running container.
	waited := createStarted("delete-waits")
	if resp, data := authedDo(t, http.MethodDelete, base, adminToken, "/api/v1/deployments/"+itoa(waited.ID), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status=%d body=%s, want 202", resp.StatusCode, data)
	}
	if got := getDeployment(t, base, adminToken, waited.ID); got.ID == 0 {
		t.Fatal("deployment disappeared before the agent confirmed the teardown")
	}
	// It still completes once the agent reports the teardown.
	waitDeploymentGone(t, base, adminToken, waited.ID, deploymentRemovalDeadline)

	// Force is the opposite guarantee, and it exists for the node that will
	// never answer: the record goes away immediately.
	forced := createStarted("delete-forced")
	resp, data := authedDo(t, http.MethodDelete, base, adminToken,
		"/api/v1/deployments/"+itoa(forced.ID)+"?force=true", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("force delete status=%d body=%s, want 200", resp.StatusCode, data)
	}
	goneResp, _ := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/deployments/"+itoa(forced.ID), nil)
	if goneResp.StatusCode != http.StatusNotFound {
		t.Fatalf("force-deleted deployment still readable: status=%d", goneResp.StatusCode)
	}
}

func waitDeploymentGone(t *testing.T, base, token string, id uint, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		resp, _ := authedDo(t, http.MethodGet, base, token, "/api/v1/deployments/"+itoa(id), nil)
		if resp.StatusCode == http.StatusNotFound {
			t.Logf("deployment %d gone after %s", id, time.Since(start))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("deployment %d was not removed within %s of the delete\n%s", id, timeout, removalDiagnostics(t, base, token, id))
}
