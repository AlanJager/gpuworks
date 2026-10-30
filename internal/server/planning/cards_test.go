package planning

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A sleeping deployment that shares its cards by service level keeps them
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
	// A resident sleeper: its card is free for anyone, as before.
	for _, level := range []string{"", servicelevel.Resident, servicelevel.Online, servicelevel.Offline} {
		if !fits(level) {
			t.Fatalf("a resident sleeper's card is closed to %q", level)
		}
	}
	if reservedAgainst(st, servicelevel.Online) != nil {
		t.Fatal("a reservation without any deployment sharing by service level")
	}

	if err := st.SetDeploymentServiceLevel(dep.ID, servicelevel.Online); err != nil {
		t.Fatal(err)
	}
	for level, want := range map[string]bool{
		"": false, servicelevel.Resident: false, servicelevel.Online: false,
		servicelevel.Shared: true, servicelevel.Offline: true,
	} {
		if got := fits(level); got != want {
			t.Fatalf("a %q newcomer on a sleeping online deployment's card fits = %v, want %v", level, got, want)
		}
	}
	// The ledger outside placement (wake admission, the board) is unchanged.
	if snap := NodeSnapshot(st, node, true, ""); snap.GPUs[0].AllocatedBytes != 0 {
		t.Fatalf("the plain ledger charges the sleeper's claim: %+v", snap.GPUs[0])
	}
}
