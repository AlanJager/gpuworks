//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// TestProcessASharingReplicaOnTwoCardsYieldsToAHigherRankAndTakesThemBack
// proves sharing across cards over the real process boundary, on a node of
// two fake 24 GiB cards. An offline (anytime) deployment's replica holds
// both cards (TP=2). An online (idle, 15 s) deployment needing one card is
// created: its replica waits for the offline one, the residency arbiter puts
// that replica to sleep whole for it, and the online replica is placed and
// answers. A request for the offline deployment is then held: once the online
// one has been idle 15 s it gives its card back, and the offline replica
// wakes on both cards and answers. The engine is the fake engine in a real
// container. Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessASharingReplicaOnTwoCardsYieldsToAHigherRankAndTakesThemBack -timeout 15m ./test/l2/
func TestProcessASharingReplicaOnTwoCardsYieldsToAHigherRankAndTakesThemBack(t *testing.T) {
	base, token, node, modelID := startDockerAutomationRigOn(t, "cardsyield", "A10x2,24576")

	// Isolate rank preemption from the default 30-second idle threshold,
	// which slow setup can cross before the second deployment is ready.
	wide, wideInst := createFakeSleepable(t, base, token, node, modelID, "cardsyield-wide", map[string]any{
		"yield_policy": map[string]any{"policy": "anytime", "idle_sec": 3600}, "placement": map[string]any{"tp": 2},
	})
	if !slices.Equal(wideInst.GPUIndexes, []int{0, 1}) {
		t.Fatalf("the offline replica holds cards %v, want 0 and 1", wideInst.GPUIndexes)
	}

	chat := postFakeSleepable(t, base, token, node, modelID, "cardsyield-chat", map[string]any{
		"yield_policy": map[string]any{"policy": "idle", "idle_sec": 15},
	})
	waitSleepLevelWithin(t, base, token, wideInst.UUID, 1, 2*time.Minute)
	chatInst := waitInstance(t, base, token, func(inst processInstance) bool {
		return inst.DeploymentID == chat && inst.State == "running" && inst.ContainerID != ""
	})
	t.Cleanup(func() {
		removeDeployment(t, base, token, chat)
		_ = exec.Command("docker", "rm", "-f", chatInst.ContainerID).Run()
	})
	if len(chatInst.GPUIndexes) != 1 {
		t.Fatalf("the online replica holds cards %v, want one", chatInst.GPUIndexes)
	}
	slept := waitEvent(t, base, token, wide, "AUTO_SLEEP")
	if slept["trigger"] != "preempt" || slept["reason"] != "rank" || slept["for_deployment"] != float64(chat) || fmt.Sprint(slept["instances"]) != fmt.Sprint([]any{wideInst.UUID}) {
		t.Fatalf("offline AUTO_SLEEP = %v, want its one replica put to sleep by rank for %d", slept, chat)
	}
	if !engineSleeping(t, wideInst.APIPort) {
		t.Fatal("the server reports the offline replica asleep but its engine is not")
	}

	key := apiKey(t, base, token, "cardsyield")
	if status, body := chatOnce(t, base, key, "cardsyield-chat", time.Minute); status != http.StatusOK {
		t.Fatalf("a request for the online deployment = %d %s", status, body)
	}

	// The offline deployment's request is held until the online one has
	// been idle its 15 s and gives the card back.
	start := time.Now()
	if status, body := chatOnce(t, base, key, "cardsyield-wide", 2*time.Minute); status != http.StatusOK {
		t.Fatalf("the held request for the offline deployment = %d %s", status, body)
	}
	t.Logf("held request answered after %v", time.Since(start).Round(time.Millisecond))
	gave := waitEvent(t, base, token, chat, "AUTO_SLEEP")
	if gave["trigger"] != "preempt" || gave["reason"] != "idle" || gave["for_deployment"] != float64(wide) || fmt.Sprint(gave["instances"]) != fmt.Sprint([]any{chatInst.UUID}) {
		t.Fatalf("online AUTO_SLEEP = %v, want its replica put to sleep once idle for %d", gave, wide)
	}
	woke := waitEvent(t, base, token, wide, "AUTO_WAKE")
	if fmt.Sprint(woke["instances"]) != fmt.Sprint([]any{wideInst.UUID}) {
		t.Fatalf("offline AUTO_WAKE = %v, want its replica woken", woke)
	}
	if engineSleeping(t, wideInst.APIPort) || !engineSleeping(t, chatInst.APIPort) {
		t.Fatal("after the swap the offline engine must be awake and the online one asleep")
	}
}

// apiKey creates an API key for the gateway.
func apiKey(t *testing.T, base, token, name string) string {
	t.Helper()
	_, data := authedDo(t, http.MethodPost, base, token, "/api/v1/apikeys", []byte(`{"name":"`+name+`"}`))
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &key); err != nil || key.Key == "" {
		t.Fatalf("api key: %s", data)
	}
	return key.Key
}

// chatOnce sends one chat completion for model through the gateway and waits
// up to within for its answer.
func chatOnce(t *testing.T, base, key, model string, within time.Duration) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: within}).Do(req)
	if err != nil {
		t.Fatalf("chat %s: %v", model, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
