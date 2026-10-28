package app

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// residencyFixture is the automation fixture's deployment asleep at L2 with
// its wake waiting, and a sleepable offline deployment awake on its card.
func residencyFixture(t *testing.T) (*residencyAdapter, *automationAdapter, *store.Store, *store.Deployment, *store.Deployment) {
	t.Helper()
	auto, st, dep := automationFixture(t)
	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "auto-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "batch", ModelID: dep.ModelID, NodeID: dep.NodeID, ServedName: "batch", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentServiceLevel(other.ID, store.ServiceLevelOffline); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "batch-1", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-1", DeploymentID: other.ID, NodeID: dep.NodeID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	px := proxy.New(st, noServingNetworks{}, nil, nil, 0)
	return &residencyAdapter{store: st, proxy: px, acct: nil}, auto, st, dep, other
}

// noServingNetworks is a cluster with no serving network: the gateway reaches
// every node through its agent.
type noServingNetworks struct{}

func (noServingNetworks) ServingEndpoints() (map[string]network.ServingEndpoint, error) {
	return nil, nil
}

func TestResidencyAdapterFindsTheHolderAndPutsItToSleep(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	if level, ok := res.Wakeable(dep.ID); !ok || level != store.ServiceLevelResident {
		t.Fatalf("an idle-slept deployment is not wakeable: %q %v", level, ok)
	}
	if _, ok := res.Wakeable(other.ID); ok {
		t.Fatal("an awake deployment reads wakeable")
	}
	if b, err := res.Blocking(dep.ID); err != nil || len(b.Holders) != 0 || b.Blocked {
		t.Fatalf("blocking before its wake waits = %+v err=%v", b, err)
	}
	result, err := auto.Wake(dep.ID)
	if err != nil || len(result.Waiting) != 1 {
		t.Fatalf("wake = %+v err=%v", result, err)
	}
	b, err := res.Blocking(dep.ID)
	if err != nil || b.Blocked || len(b.Holders) != 1 {
		t.Fatalf("blocking = %+v err=%v", b, err)
	}
	h := b.Holders[0]
	if h.DeploymentID != other.ID || h.Name != "batch" || h.Level != store.ServiceLevelOffline || !slices.Equal(h.Instances, []string{"batch-1"}) {
		t.Fatalf("holder = %+v", h)
	}

	level, err := res.Yield(other.ID, h.Instances, true)
	if err != nil || level != proto.SleepLevelL2 || intentOf(t, st, "batch-1") != proto.SleepLevelL2 {
		t.Fatalf("yield = %d err=%v intent=%d", level, err, intentOf(t, st, "batch-1"))
	}
	in, _ := st.InstanceByUUID("batch-1")
	var desired proto.InstanceDesiredState
	_ = json.Unmarshal([]byte(in.DesiredStateJSON), &desired)
	if desired.SleepMode != proto.SleepModeAbort {
		t.Fatalf("an offline holder's sleep mode = %q, want abort", desired.SleepMode)
	}
	if got, _ := st.GetDeployment(other.ID); got.SleepHeld {
		t.Fatal("the arbiter's sleep is held: a request would not wake the holder again")
	}
	// Once the holder reports asleep, the waiting wake fits.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	if woken, err := auto.WakeWaiting(dep.ID, result.Waiting); err != nil || len(woken) != 1 {
		t.Fatalf("waiting wake after the yield = %v err=%v", woken, err)
	}
	if level, ok := res.Wakeable(other.ID); !ok || level != store.ServiceLevelOffline {
		t.Fatalf("the holder put to sleep for another is not wakeable: %q %v", level, ok)
	}
}

func TestResidencyAdapterBlockingAndHeldSleeps(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	// An occupant that cannot sleep blocks the wake.
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", other.ID).Update("sleepable", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := auto.Wake(dep.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := res.Blocking(dep.ID); err != nil || !b.Blocked {
		t.Fatalf("blocking with an occupant that cannot sleep = %+v err=%v", b, err)
	}
	// An operator's sleep is not the arbiter's to end.
	if err := st.OrderDeploymentSleepHeld(dep.ID, 2, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Wakeable(dep.ID); ok {
		t.Fatal("a held sleep reads wakeable")
	}
	if waiters, err := res.Waiters(); err != nil || len(waiters) != 0 {
		t.Fatalf("waiters with nothing held = %+v err=%v", waiters, err)
	}
	if act := res.Activity(dep.ID); act.InFlight != 0 || act.Held != 0 || !act.LastRequest.IsZero() {
		t.Fatalf("activity with no traffic = %+v", act)
	}
	res.Drain(other.ID, true)
	res.Drain(other.ID, false)
}
