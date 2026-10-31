//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"testing"
	"time"
)

// TestProcessARequestWakesOnlyTheReplicaThePlatformSlept proves that holding
// a sleep is a fact of each replica, across the real process boundary. A
// two-replica deployment is put to sleep by its operator; an offline
// deployment takes the card of one of them (b), whose wake then waits for
// room, which lifts b's hold alone. A request is held by the gateway, the
// residency arbiter puts the offline deployment to sleep and wakes b; the
// other replica (a) stays asleep, held. The engine is the fake engine in a
// real container. Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessARequestWakesOnlyTheReplicaThePlatformSlept -timeout 15m ./test/l2/
func TestProcessARequestWakesOnlyTheReplicaThePlatformSlept(t *testing.T) {
	base, token, node, modelID := startDockerAutomationRigOn(t, "held-replicas", "A100x2,24576")

	chat := postFakeSleepable(t, base, token, node, modelID, "held-chat", map[string]any{"replicas": 2, "yield_policy": map[string]any{"policy": "idle"}})
	running := func(skip string) processInstance {
		return waitInstance(t, base, token, func(inst processInstance) bool {
			return inst.DeploymentID == chat && inst.UUID != skip && inst.State == "running" && inst.ContainerID != "" && len(inst.Argv) > 0
		})
	}
	first := running("")
	second := running(first.UUID)
	t.Cleanup(func() {
		removeDeployment(t, base, token, chat)
		_ = exec.Command("docker", "rm", "-f", first.ContainerID, second.ContainerID).Run()
	})
	if resp, data := authedDo(t, http.MethodPost, base, token, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=2", chat), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator sleep status=%d body=%s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, base, token, first.UUID, 2, time.Minute)
	waitSleepLevelWithin(t, base, token, second.UUID, 2, time.Minute)

	// The offline deployment lands on a card a held sleeper keeps nothing on.
	_, batchInst := createFakeSleepable(t, base, token, node, modelID, "held-batch", map[string]any{"yield_policy": map[string]any{"policy": "anytime"}})
	a, b := first, second
	if len(batchInst.GPUIndexes) == 1 && len(first.GPUIndexes) == 1 && batchInst.GPUIndexes[0] == first.GPUIndexes[0] {
		a, b = second, first
	}
	resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/instances/"+b.UUID+"/wake?wait=true", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("b's wake that waits for room = %d %s", resp.StatusCode, data)
	}
	if got := deploymentField(t, base, token, chat, "sleep_hold"); got != "some" {
		t.Fatalf("sleep_hold = %v with a held and a waiting replica, want some", got)
	}

	_, keyData := authedDo(t, http.MethodPost, base, token, "/api/v1/apikeys", []byte(`{"name":"held-replicas"}`))
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(keyData, &key); err != nil || key.Key == "" {
		t.Fatalf("api key: %s", keyData)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"held-chat","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Content-Type", "application/json")
	answer, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(answer.Body)
	_ = answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the held request = %d %s, want it answered once b woke", answer.StatusCode, body)
	}
	waitSleepLevelWithin(t, base, token, b.UUID, 0, 30*time.Second)
	if engineSleeping(t, b.APIPort) {
		t.Fatal("the request was answered but b's engine still sleeps")
	}
	if !engineSleeping(t, a.APIPort) {
		t.Fatal("a request woke a, which its operator holds asleep")
	}
	if got := instanceField(t, base, token, a.UUID, "sleep_held"); got != true {
		t.Fatalf("a's sleep_held = %v after the request, want true", got)
	}
}

// instanceField is one top-level field of the instance's wire.
func instanceField(t *testing.T, base, token, uuid, field string) any {
	t.Helper()
	_, data := authedDo(t, http.MethodGet, base, token, "/api/v1/instances/"+uuid, nil)
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("instance %s: %v (%s)", uuid, err, data)
	}
	return wire[field]
}
