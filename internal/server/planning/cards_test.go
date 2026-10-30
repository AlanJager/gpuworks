package planning

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A sleeping deployment that shares its cards by yield policy keeps them
// against a newcomer it does not share with, and gives them to one it does.
func TestASleepingSharersCardsAreReservedAgainstLevelsItDoesNotShareWith(t *testing.T) {
	st, err := store.Open("file:planning_reserved_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("reserved-node", store.HashToken("token"), "reserved-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "chat", ModelID: 1, NodeID: node.ID, ServedName: "chat", Engine: "vllm", Sleepable: true, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "chat-1", DeploymentID: dep.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("chat-1", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "chat-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{WeightBytes: 8 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm")}
	fits := func(level string) bool {
		t.Helper()
		snap := nodeSnapshot(st, node, true, "", ledgerView{reserved: reservedAgainst(st, level)})
		return scheduler.Schedule(req, snap).OK
	}
	// A never sleeper: its card is free for anyone, as before.
	for _, level := range []string{"", servicelevel.Never, servicelevel.Idle, servicelevel.Anytime} {
		if !fits(level) {
			t.Fatalf("a never sleeper's card is closed to %q", level)
		}
	}
	if reservedAgainst(st, servicelevel.Idle) != nil {
		t.Fatal("a reservation without any deployment sharing by yield policy")
	}

	if err := st.SetDeploymentYieldPolicy(dep.ID, servicelevel.Policy{Yield: servicelevel.Idle, IdleSec: 300}); err != nil {
		t.Fatal(err)
	}
	for level, want := range map[string]bool{
		"": false, servicelevel.Never: false, servicelevel.Idle: true,
		servicelevel.TimeSlice: true, servicelevel.Anytime: true,
	} {
		if got := fits(level); got != want {
			t.Fatalf("a %q newcomer on a sleeping idle deployment's card fits = %v, want %v", level, got, want)
		}
	}
	// The ledger outside placement (wake admission, the board) is unchanged.
	if snap := NodeSnapshot(st, node, true, ""); snap.GPUs[0].AllocatedBytes != 0 {
		t.Fatalf("the plain ledger charges the sleeper's claim: %+v", snap.GPUs[0])
	}

	// A never newcomer is refused by name: it would fit but for the sleeper.
	view := ledgerView{reserved: reservedAgainst(st, servicelevel.Never)}
	reserved := launchReserved(st, node, nodeInstances(t, st, node), req, true, "", false, view)
	if reserved == nil || len(reserved.Sleepers) != 1 || reserved.Sleepers[0].Deployment != "chat" || len(reserved.GPUIndexes) != 1 || reserved.GPUIndexes[0] != 0 {
		t.Fatalf("reserved = %+v, want chat asleep on card 0", reserved)
	}
	if reserved.Sleepers[0].DeploymentID != dep.ID || reserved.Sleepers[0].Policy.Yield != servicelevel.Idle {
		t.Fatalf("sleeper = %+v", reserved.Sleepers[0])
	}
	// An edit preview of chat itself does not name its own sleeper.
	own := ledgerView{excludeDeployment: dep.ID, reserved: reservedAgainst(st, servicelevel.Never)}
	if got := launchReserved(st, node, nodeInstances(t, st, node), req, true, "", false, own); got != nil {
		t.Fatalf("an edit preview names its own sleeper: %+v", got)
	}
	reason := ReservedRefusal(node, reserved)
	if raw, _ := json.Marshal(reason.Params); reason.Code != scheduler.ReasonReservedForSleepers ||
		string(raw) != `{"gpus":[0],"node":"reserved-node","sleepers":[{"deployment":"chat","policy":"idle"}]}` {
		t.Fatalf("refusal = %s %s", reason.Code, raw)
	}

	// An operator's sleep is held: no request wakes it, so it keeps nothing
	// and the card is anyone's.
	if err := st.OrderDeploymentSleepHeld(dep.ID, 2, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	st.Bus.PublishChange("deployment", fmt.Sprint(dep.ID), "test")
	for _, level := range []string{"", servicelevel.Never, servicelevel.Idle} {
		if !fits(level) {
			t.Fatalf("a %q newcomer is kept off a held sleeper's card", level)
		}
	}
	if launchReserved(st, node, nodeInstances(t, st, node), req, true, "", false, ledgerView{reserved: reservedAgainst(st, servicelevel.Never)}) != nil {
		t.Fatal("a held sleeper is named as reserving its card")
	}
}
