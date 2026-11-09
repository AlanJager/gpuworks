//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestProcessARequestTakesTheCardsBackFromAnOfflineDeployment proves on-demand
// wake and the residency arbiter across the real process boundary. An online
// deployment shares its one card with an offline deployment placed while it
// slept, and its wake waits for room. A request for the online one
// is held by the gateway, the arbiter puts the offline one to sleep, the
// online one wakes on the engine, and the held request is answered. An
// operator's sleep, by contrast, is held: a request gets 503. The engine is
// the fake engine in a real container. Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessARequestTakesTheCardsBackFromAnOfflineDeployment -timeout 15m ./test/l2/
func TestProcessARequestTakesTheCardsBackFromAnOfflineDeployment(t *testing.T) {
	base, token, node, modelID := startDockerAutomationRig(t, "residency")

	create := func(served string, extra map[string]any) (uint, processInstance) {
		t.Helper()
		return createFakeSleepable(t, base, token, node, modelID, served, extra)
	}

	// The online deployment, asleep, and the offline one on the card it gave
	// up; the online one's wake then waits for room, so a request may wake it.
	chat, chatInst := create("residency-chat", map[string]any{"yield_policy": map[string]any{"policy": "idle"}})
	sleepForTheTaking(t, base, token, chat, chatInst.UUID)
	batch, batchInst := create("residency-batch", map[string]any{"yield_policy": map[string]any{"policy": "anytime"}})
	wakeWhenThereIsRoom(t, base, token, chat)

	_, keyData := authedDo(t, http.MethodPost, base, token, "/api/v1/apikeys", []byte(`{"name":"residency"}`))
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(keyData, &key); err != nil || key.Key == "" {
		t.Fatalf("api key: %s", keyData)
	}
	ask := func(model string, within time.Duration) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
			bytes.NewBufferString(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: within}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("chat %s: %v", model, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	start := time.Now()
	if status, body := ask("residency-chat", 2*time.Minute); status != http.StatusOK {
		t.Fatalf("the held request = %d %s, want it answered once the deployment woke", status, body)
	}
	t.Logf("held request answered after %v", time.Since(start).Round(time.Millisecond))
	for _, id := range []uint{chat, batch} {
		_, data := authedDo(t, http.MethodGet, base, token, fmt.Sprintf("/api/v1/deployments/%d/events?limit=50", id), nil)
		var events struct {
			Items []struct {
				Code      string         `json:"code"`
				CreatedAt time.Time      `json:"created_at"`
				Params    map[string]any `json:"params"`
			} `json:"items"`
		}
		_ = json.Unmarshal(data, &events)
		for _, ev := range events.Items {
			if ev.CreatedAt.After(start.Add(-time.Second)) {
				t.Logf("deployment %d +%v %s %v", id, ev.CreatedAt.Sub(start).Round(time.Millisecond), ev.Code, ev.Params)
			}
		}
	}
	waitSleepLevelWithin(t, base, token, batchInst.UUID, 1, 30*time.Second)
	if !engineSleeping(t, batchInst.APIPort) {
		t.Fatal("the server reports the offline deployment asleep but its engine is not")
	}
	if engineSleeping(t, chatInst.APIPort) {
		t.Fatal("the online deployment answered but its engine still sleeps")
	}
	if params := waitEvent(t, base, token, batch, "AUTO_SLEEP"); params["trigger"] != "preempt" || params["for_deployment"] != float64(chat) {
		t.Fatalf("offline AUTO_SLEEP params = %v, want trigger preempt for %d", params, chat)
	}
	if params := waitEvent(t, base, token, chat, "AUTO_WAKE"); params["trigger"] == "schedule" {
		t.Fatalf("online AUTO_WAKE params = %v", params)
	}

	// An operator's sleep of a deployment that shares its cards is, by
	// default, one a request wakes: the request is held and answered.
	if resp, data := authedDo(t, http.MethodPost, base, token, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=2", chat), nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"wake_on_request":true`) {
		t.Fatalf("operator sleep status=%d body=%s, want one a request wakes", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, token, chatInst.UUID, 2, time.Minute)
	if status, body := ask("residency-chat", 2*time.Minute); status != http.StatusOK {
		t.Fatalf("a request for an operator's sleep a request wakes = %d %s, want it answered", status, body)
	}

	// An operator's sleep the order holds is refused: a request gets 503.
	if resp, data := authedDo(t, http.MethodPost, base, token, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=2&wake_on_request=false", chat), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("held operator sleep status=%d body=%s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, token, chatInst.UUID, 2, time.Minute)
	if status, body := ask("residency-chat", 30*time.Second); status != http.StatusServiceUnavailable || !strings.Contains(body, "asleep") {
		t.Fatalf("a request for an operator's held sleep = %d %s, want 503 asleep", status, body)
	}
}

// sleepForTheTaking puts the deployment to sleep at L2, an operator's held
// sleep, which keeps nothing on its cards, so another may be placed on them.
func sleepForTheTaking(t *testing.T, base, token string, deploymentID uint, instanceUUID string) {
	t.Helper()
	if resp, data := authedDo(t, http.MethodPost, base, token, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=2&wake_on_request=false", deploymentID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator sleep status=%d body=%s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, token, instanceUUID, 2, time.Minute)
}

// wakeWhenThereIsRoom orders the sleeping deployment's wake, which waits for
// room another deployment holds: the sleep is no longer an operator's, so a
// request for it makes the residency arbiter look.
func wakeWhenThereIsRoom(t *testing.T, base, token string, deploymentID uint) {
	t.Helper()
	resp, data := authedDo(t, http.MethodPost, base, token, fmt.Sprintf("/api/v1/deployments/%d/wake?wait=true", deploymentID), nil)
	var result struct {
		Status  string   `json:"status"`
		Woken   []string `json:"woken"`
		Waiting []string `json:"waiting"`
	}
	if err := json.Unmarshal(data, &result); err != nil || resp.StatusCode != http.StatusAccepted || result.Status != "waiting" || len(result.Woken) != 0 || len(result.Waiting) != 1 {
		t.Fatalf("wake that waits for room = %d %s, want one waiting", resp.StatusCode, data)
	}
	if got := deploymentField(t, base, token, deploymentID, "sleep_held"); got != false {
		t.Fatalf("sleep_held = %v after the wake order, want false", got)
	}
}

// deploymentField is one top-level field of the deployment's wire.
func deploymentField(t *testing.T, base, token string, deploymentID uint, field string) any {
	t.Helper()
	_, data := authedDo(t, http.MethodGet, base, token, fmt.Sprintf("/api/v1/deployments/%d", deploymentID), nil)
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("deployment %d: %v (%s)", deploymentID, err, data)
	}
	return wire[field]
}

// createFakeSleepable creates a sleepable fake-engine deployment of served on
// node, with the extra request fields, and waits for its instance to run.
// When the test ends the deployment is deleted, its container removed as a
// last resort.
func createFakeSleepable(t *testing.T, base, token, node string, modelID uint, served string, extra map[string]any) (uint, processInstance) {
	t.Helper()
	id := postFakeSleepable(t, base, token, node, modelID, served, extra)
	inst := waitInstance(t, base, token, func(inst processInstance) bool {
		return inst.DeploymentID == id && inst.State == "running" && inst.ContainerID != "" && len(inst.Argv) > 0
	})
	t.Cleanup(func() {
		removeDeployment(t, base, token, id)
		_ = exec.Command("docker", "rm", "-f", inst.ContainerID).Run()
	})
	return id, inst
}

// postFakeSleepable creates the deployment without waiting for it to run.
func postFakeSleepable(t *testing.T, base, token, node string, modelID uint, served string, extra map[string]any) uint {
	t.Helper()
	body := map[string]any{
		"model_id": modelID, "node_uuid": node, "served_name": served,
		"engine": "vllm", "image": "gpuworks/fake-engine:test", "sleepable": true,
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/deployments", raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s status=%d body=%s", served, resp.StatusCode, data)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// removeDeployment deletes the deployment and waits until it is gone.
func removeDeployment(t *testing.T, base, token string, id uint) {
	t.Helper()
	deleteDeployment(t, base, token, id)
}
