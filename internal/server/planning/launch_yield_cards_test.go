package planning

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// yieldCards is a node of cards 16 GiB each and the sharing deployments on
// it, for launches that span or overlap several cards.
type yieldCards struct {
	st   *store.Store
	node *store.Node
}

func newYieldCards(t *testing.T, cards int) *yieldCards {
	t.Helper()
	st, err := store.Open("file:planning_yield_cards_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("cards-node", store.HashToken("token"), "cards-node")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cards; i++ {
		node.GPUs = append(node.GPUs, store.GPU{Index: i, UUID: "gpu-" + string(rune('0'+i)), Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30})
	}
	return &yieldCards{st: st, node: node}
}

func (c *yieldCards) deployment(t *testing.T, name, yield string) *store.Deployment {
	t.Helper()
	dep := &store.Deployment{Name: name, ModelID: 1, NodeID: c.node.ID, ServedName: name, Engine: "vllm", Sleepable: true, State: "active", Replicas: 2}
	p, _ := yieldpolicy.Parse(yield, yieldpolicy.Overrides{})
	dep.SetPolicy(p)
	created, err := c.st.CreateDeployment(dep)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (c *yieldCards) instance(t *testing.T, dep *store.Deployment, uuid, gpus string, sleepLevel int) {
	t.Helper()
	in := &store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: c.node.ID, State: "running", GPUIndexes: gpus, SleepLevel: sleepLevel, DesiredStateJSON: sleepSpec(uuid, sleepLevel)}
	if _, err := c.st.CreateInstance(in); err != nil {
		t.Fatal(err)
	}
}

// launch is launchYield for a deployment of policy (launching, zero for a
// newcomer) needing gpus cards of 8 GiB weights each.
func (c *yieldCards) launch(t *testing.T, launching uint, policy string, gpus int) *LaunchYield {
	t.Helper()
	req := scheduler.Request{WeightBytes: int64(gpus) * 8 << 30, HeadCount: 8, GPUMemUtil: deploy.DefaultGPUMemUtil("vllm"), AllowedTPs: []int{gpus}}
	view := ledgerView{reserved: reservedAgainst(c.st, policy)}
	return launchYield(c.st, c.node, nodeInstances(t, c.st, c.node), launching, policy, req, true, "", false, view)
}

func holderNames(y *LaunchYield) []string {
	if y == nil {
		return nil
	}
	out := y.HolderInstances()
	slices.Sort(out)
	return out
}

// A second replica of a sharing deployment never waits for its own first
// replica: it is not a holder and is not counted asleep, so the launch takes
// the card another deployment would give up, or waits for no one.
func TestALaunchNeverWaitsForItsOwnDeployment(t *testing.T) {
	c := newYieldCards(t, 2)
	chat := c.deployment(t, "chat", yieldpolicy.Idle)
	batch := c.deployment(t, "batch", yieldpolicy.Anytime)
	c.instance(t, chat, "chat-1", "[0]", 0)
	c.instance(t, batch, "batch-1", "[1]", 0)
	// chat's second replica takes card 1, which batch gives up, never card 0
	// its own first replica holds.
	yield := c.launch(t, chat.ID, yieldpolicy.Idle, 1)
	if yield == nil || !slices.Equal(holderNames(yield), []string{"batch-1"}) || !slices.Equal(yield.GPUIndexes, []int{1}) {
		t.Fatalf("chat's second replica = %+v, want it to wait for batch-1 on card 1", yield)
	}
	// A newcomer of the same policy may take either; the lower card first.
	if newcomer := c.launch(t, 0, yieldpolicy.Idle, 1); newcomer == nil || !slices.Equal(newcomer.GPUIndexes, []int{0}) || !slices.Equal(holderNames(newcomer), []string{"chat-1"}) {
		t.Fatalf("a newcomer = %+v, want card 0 from chat-1", newcomer)
	}
	// With chat alone on both cards, its second replica waits for no one.
	if err := c.st.DB().Delete(&store.Instance{}, "uuid = ?", "batch-1").Error; err != nil {
		t.Fatal(err)
	}
	if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", "chat-1").Select("GPUIndexes").Updates(&store.Instance{GPUIndexes: "[0,1]"}).Error; err != nil {
		t.Fatal(err)
	}
	if got := c.launch(t, chat.ID, yieldpolicy.Idle, 1); got != nil {
		t.Fatalf("chat's second replica waits for its own first: %+v", got)
	}
}

// A sharing instance still starting on the card holds it as one running
// does: the launch waits for it (the arbiter asks it nothing until it
// serves), as a wake waits for a starting occupant (YieldTargets). One that
// failed holds nothing.
func TestALaunchWaitsForAHolderStillStarting(t *testing.T) {
	for _, state := range []string{proto.InstPending, proto.InstPulling, proto.InstStarting, proto.InstOptimizing} {
		t.Run(state, func(t *testing.T) {
			c := newYieldCards(t, 1)
			batch := c.deployment(t, "batch", yieldpolicy.Anytime)
			c.instance(t, batch, "batch-1", "[0]", 0)
			if err := c.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: state}); err != nil {
				t.Fatal(err)
			}
			if got := c.launch(t, 0, yieldpolicy.Idle, 1); got == nil || !slices.Equal(holderNames(got), []string{"batch-1"}) {
				t.Fatalf("a launch beside batch %s = %+v, want it to wait for batch-1", state, got)
			}
		})
	}
	c := newYieldCards(t, 1)
	batch := c.deployment(t, "batch", yieldpolicy.Anytime)
	c.instance(t, batch, "batch-1", "[0]", 0)
	if err := c.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstFailed}); err != nil {
		t.Fatal(err)
	}
	if got := c.launch(t, 0, yieldpolicy.Idle, 1); got != nil {
		t.Fatalf("a launch beside a failed holder = %+v", got)
	}
}

// The controller's launch names its deployment, which is how launchYield
// knows whose instances are its own.
func TestALaunchInputNamesItsDeployment(t *testing.T) {
	dep := &store.Deployment{ID: 7, Engine: "vllm"}
	in, err := launchInput(nil, LaunchRequest{Source: LaunchSourceFromDeployment(dep), Node: &store.Node{}})
	if err != nil || in.DeploymentID != 7 {
		t.Fatalf("input = %d err=%v, want deployment 7", in.DeploymentID, err)
	}
}
