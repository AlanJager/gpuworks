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
// deployment the platform put to sleep (auto sleep) shares its one card with
// an offline deployment placed while it slept. A request for the online one
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
		inst := waitInstance(t, base, token, func(inst processInstance) bool {
			return inst.DeploymentID == created.ID && inst.State == "running" && inst.ContainerID != "" && len(inst.Argv) > 0
		})
		t.Cleanup(func() {
			if resp, err := authedDo(t, http.MethodDelete, base, token, "/api/v1/deployments/"+itoa(created.ID), nil); err == nil && resp != nil {
				resp.Body.Close()
			}
			waitDeploymentGone(t, base, token, created.ID, 30*time.Second)
			_ = exec.Command("docker", "rm", "-f", inst.ContainerID).Run()
		})
		return created.ID, inst
	}
	put := func(path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		if resp, data := authedDo(t, http.MethodPut, base, token, path, raw); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s status=%d body=%s", path, resp.StatusCode, data)
		}
	}

	// The online deployment, put to sleep by auto sleep: a sleep the platform
	// chose. Setting the online preset again then ends the auto sleep.
	chat, chatInst := create("residency-chat", map[string]any{"yield_policy": map[string]any{"preset": "online"}})
	put(fmt.Sprintf("/api/v1/deployments/%d/yield-policy", chat), map[string]any{"policy": "idle", "auto_sleep": true, "idle_sec": 60, "sleep_level": "l2"})
	waitSleepLevelWithin(t, base, token, chatInst.UUID, 2, 3*time.Minute)
	put(fmt.Sprintf("/api/v1/deployments/%d/yield-policy", chat), map[string]any{"preset": "online"})

	// The offline deployment takes the card the sleeper gave up.
	batch, batchInst := create("residency-batch", map[string]any{"yield_policy": map[string]any{"preset": "offline"}})

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

	// An operator's sleep is held: a request is refused, not held.
	if resp, data := authedDo(t, http.MethodPost, base, token, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=2", chat), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator sleep status=%d body=%s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, token, chatInst.UUID, 2, time.Minute)
	if status, body := ask("residency-chat", 30*time.Second); status != http.StatusServiceUnavailable || !strings.Contains(body, "asleep") {
		t.Fatalf("a request for an operator's sleep = %d %s, want 503 asleep", status, body)
	}
}
