package planning

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// targetsRig is a node of two 48 GiB cards for a wake that does not fit and
// the occupants it would put to sleep.
type targetsRig struct {
	st   *store.Store
	node *store.Node
}

func newTargetsRig(t *testing.T) *targetsRig {
	t.Helper()
	st, err := store.Open("file:planning_yield_targets_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("targets-node", store.HashToken("token"), "targets-node")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		gpu := store.GPU{NodeID: node.ID, Index: i, UUID: fmt.Sprintf("gpu-%d", i), Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 48 << 30}
		if err := st.DB().Create(&gpu).Error; err != nil {
			t.Fatal(err)
		}
		node.GPUs = append(node.GPUs, gpu)
	}
	return &targetsRig{st: st, node: node}
}

func (r *targetsRig) deployment(t *testing.T, name string, replicas int, sleepable bool) *store.Deployment {
	t.Helper()
	dep, err := r.st.CreateDeployment(&store.Deployment{Name: name, ModelID: 1, NodeID: r.node.ID, ServedName: name, Engine: "vllm", Sleepable: sleepable, State: store.DeployActive, Replicas: replicas})
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

// instance is a replica on gpus admitted with 40 GiB of each card, asleep at
// level when it is above 0.
func (r *targetsRig) instance(t *testing.T, dep *store.Deployment, uuid, gpus string, level int) {
	t.Helper()
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: dep.Sleepable, MemoryClaimBytes: 40 << 30})
	if _, err := r.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: r.node.ID, State: proto.InstRunning, GPUIndexes: gpus, NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	if level > 0 {
		if err := r.st.OrderInstancesSleep(dep.ID, []string{uuid}, level, ""); err != nil {
			t.Fatal(err)
		}
		if err := r.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: level}); err != nil {
			t.Fatal(err)
		}
	}
}

// targets admits the wake of dep's sleepers and returns what a yield would
// put to sleep for the refused ones: each target as deployment:instance
// [members], and each blocker as deployment/code.
func (r *targetsRig) targets(t *testing.T, dep *store.Deployment) ([]string, []string) {
	t.Helper()
	insts, err := r.st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	match, err := DeploymentMatch(r.st, dep, insts)
	if err != nil {
		t.Fatal(err)
	}
	_, refused, err := AdmitWake(r.st, WakeGroups(match, Sleepers(insts)), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(refused) == 0 {
		t.Fatal("the wake fits: nothing to yield")
	}
	targets, blockers, err := YieldTargets(r.st, refused)
	if err != nil {
		t.Fatal(err)
	}
	var got, blocked []string
	for _, target := range targets {
		var members []string
		for _, m := range target.Members {
			members = append(members, m.UUID)
		}
		got = append(got, fmt.Sprintf("%s:%s %v", target.Deployment.Name, target.Instance.UUID, members))
	}
	for _, b := range blockers {
		blocked = append(blocked, b.Deployment+"/"+b.Code)
	}
	return got, blocked
}

// An occupant replica on two cards (TP=2) of which the wake needs one is a
// target once, whole; needing both its cards, still once.
func TestYieldTargetsTakeAReplicaSpanningCardsOnce(t *testing.T) {
	r := newTargetsRig(t)
	wide := r.deployment(t, "wide", 1, true)
	narrow := r.deployment(t, "narrow", 1, true)
	big := r.deployment(t, "big", 1, true)
	r.instance(t, wide, "wide-1", "[0,1]", 0)
	r.instance(t, narrow, "narrow-1", "[1]", 1)
	r.instance(t, big, "big-1", "[0,1]", 1)
	for _, waker := range []*store.Deployment{narrow, big} {
		got, blocked := r.targets(t, waker)
		if !slices.Equal(got, []string{"wide:wide-1 [wide-1]"}) || len(blocked) != 0 {
			t.Fatalf("waking %s: targets %v blockers %v, want wide-1 once", waker.Name, got, blocked)
		}
	}
}

// An occupant deployment with a replica on each needed card puts each of
// them to sleep, each once; a replica of it on a card the wake does not need
// stays awake.
func TestYieldTargetsTakeEachReplicaOnTheNeededCards(t *testing.T) {
	r := newTargetsRig(t)
	pair := r.deployment(t, "pair", 2, true)
	big := r.deployment(t, "big", 1, true)
	narrow := r.deployment(t, "narrow", 1, true)
	r.instance(t, pair, "pair-1", "[0]", 0)
	r.instance(t, pair, "pair-2", "[1]", 0)
	r.instance(t, big, "big-1", "[0,1]", 1)
	r.instance(t, narrow, "narrow-1", "[1]", 1)
	got, blocked := r.targets(t, big)
	slices.Sort(got)
	if !slices.Equal(got, []string{"pair:pair-1 [pair-1]", "pair:pair-2 [pair-2]"}) || len(blocked) != 0 {
		t.Fatalf("waking big: targets %v blockers %v, want pair-1 and pair-2 once each", got, blocked)
	}
	if got, _ := r.targets(t, narrow); !slices.Equal(got, []string{"pair:pair-2 [pair-2]"}) {
		t.Fatalf("waking narrow: targets %v, want pair-2 alone", got)
	}
}

// An occupant already ordered to sleep, its sleep not landed yet, is no
// target: the wake waits for that sleep, and is not ordered (nor counted as
// a yield) again.
func TestYieldTargetsLeaveASleepAlreadyOrdered(t *testing.T) {
	r := newTargetsRig(t)
	pair := r.deployment(t, "pair", 2, true)
	big := r.deployment(t, "big", 1, true)
	r.instance(t, pair, "pair-1", "[0]", 0)
	r.instance(t, pair, "pair-2", "[1]", 0)
	r.instance(t, big, "big-1", "[0,1]", 1)
	if err := r.st.OrderInstancesSleep(pair.ID, []string{"pair-2"}, 1, ""); err != nil {
		t.Fatal(err)
	}
	if got, blocked := r.targets(t, big); !slices.Equal(got, []string{"pair:pair-1 [pair-1]"}) || len(blocked) != 0 {
		t.Fatalf("targets %v blockers %v, want pair-1 alone", got, blocked)
	}
}

// An occupant that cannot sleep blocks the yield, named once though it holds
// both needed cards.
func TestYieldTargetsNameWhatCannotSleep(t *testing.T) {
	r := newTargetsRig(t)
	fixed := r.deployment(t, "fixed", 1, false)
	big := r.deployment(t, "big", 1, true)
	r.instance(t, fixed, "fixed-1", "[0,1]", 0)
	r.instance(t, big, "big-1", "[0,1]", 1)
	got, blocked := r.targets(t, big)
	if len(got) != 0 || !slices.Equal(blocked, []string{"fixed/" + sleep.CodeNotEnabled}) {
		t.Fatalf("targets %v blockers %v, want fixed blocking once", got, blocked)
	}
}
