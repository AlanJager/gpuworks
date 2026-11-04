//go:build multiproc

package l2

import (
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// TestProcessANewOnlineDeploymentTakesTheCardFromAnOfflineOne proves that a
// new deployment waits for the ones sharing its card by yield policy, across
// the real process boundary. An offline (anytime) deployment holds the one
// card awake; an online (idle) deployment is created on the same node. Its
// replica does not fit, the controller records that it waits for the offline
// one, the residency arbiter puts that one to sleep at once (it ranks lower),
// and the online replica is then placed and runs. The engine is the fake
// engine in a real container. Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessANewOnlineDeploymentTakesTheCardFromAnOfflineOne -timeout 15m ./test/l2/
func TestProcessANewOnlineDeploymentTakesTheCardFromAnOfflineOne(t *testing.T) {
	base, token, node, modelID := startDockerAutomationRig(t, "placeyield")

	// Keep this handoff in the rank branch even when setup crosses the
	// default 30-second idle threshold.
	batch, batchInst := createFakeSleepable(t, base, token, node, modelID, "placeyield-batch", map[string]any{"yield_policy": map[string]any{"policy": "anytime", "idle_sec": 3600}})

	start := time.Now()
	chat := postFakeSleepable(t, base, token, node, modelID, "placeyield-chat", map[string]any{"yield_policy": map[string]any{"policy": "idle"}})
	waitSleepLevelWithin(t, base, token, batchInst.UUID, 1, 2*time.Minute)
	chatInst := waitInstance(t, base, token, func(inst processInstance) bool {
		return inst.DeploymentID == chat && inst.State == "running" && inst.ContainerID != ""
	})
	t.Cleanup(func() {
		removeDeployment(t, base, token, chat)
		_ = exec.Command("docker", "rm", "-f", chatInst.ContainerID).Run()
	})
	t.Logf("the online replica ran %v after it was created", time.Since(start).Round(time.Millisecond))

	waiting := waitEvent(t, base, token, chat, "DEPLOYMENT_UNSCHEDULABLE")
	scheduled, _ := waiting["scheduler"].(map[string]any)
	if scheduled["code"] != "waiting_for_yield" {
		t.Fatalf("the online replica's refusal = %v, want waiting_for_yield", waiting)
	}
	params, _ := scheduled["params"].(map[string]any)
	holders, _ := params["holders"].([]any)
	if len(holders) != 1 || fmt.Sprint(holders[0].(map[string]any)["deployment"]) != "placeyield-batch" || holders[0].(map[string]any)["at_once"] != true {
		t.Fatalf("waiting for %v, want the batch deployment, at once", params["holders"])
	}
	slept := waitEvent(t, base, token, batch, "AUTO_SLEEP")
	if slept["trigger"] != "preempt" || slept["reason"] != "rank" || slept["for_deployment"] != float64(chat) {
		t.Fatalf("batch AUTO_SLEEP = %v, want a preempt by rank for %d", slept, chat)
	}
	if !engineSleeping(t, batchInst.APIPort) {
		t.Fatal("the server reports the offline deployment asleep but its engine is not")
	}
}
