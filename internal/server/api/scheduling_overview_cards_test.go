package api

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A replica awake on two cards (TP=2) and another deployment asleep on one
// of them: both cards read awake, the awake replica is on both, the sleeper
// only on its own card with the residue it keeps there, and the summary
// counts each card once.
func TestSchedulingOverviewOfAReplicaSpanningCardsBesideASleeper(t *testing.T) {
	rig := newOverviewRig(t)
	before, _ := rig.overview(t)
	n, err := rig.st.RegisterNode("gpu-d", store.HashToken("gpu-d"), "gpu-d")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := rig.st.DB().Create(&store.GPU{NodeID: n.ID, Index: i, Ident: fmt.Sprintf("gpu-d-%d", i), Name: "L20", MemoryTotal: 48 * gib}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name, policy string) *store.Deployment {
		dep, err := rig.st.CreateDeployment(presetDeployment(&store.Deployment{ModelID: 1, NodeID: n.ID, Name: name, ServedName: name, Engine: "vllm", Sleepable: true, State: store.DeployActive}, policy))
		if err != nil {
			t.Fatal(err)
		}
		return dep
	}
	wide, side := mk("wide", yieldpolicy.Anytime), mk("side", yieldpolicy.TimeSlice)
	for _, in := range []store.Instance{
		{UUID: "wide-d", DeploymentID: wide.ID, NodeID: n.ID, State: proto.InstRunning, GPUIndexes: "[0,1]", DesiredStateJSON: `{"desired_sleep":0,"memory_claim_bytes":` + fmt.Sprint(24*gib) + `}`},
		{UUID: "side-d", DeploymentID: side.ID, NodeID: n.ID, State: proto.InstRunning, GPUIndexes: "[1]", SleepLevel: 1, SleepResidueBytes: 2 * gib, DesiredStateJSON: `{"desired_sleep":1,"memory_claim_bytes":` + fmt.Sprint(12*gib) + `}`},
	} {
		in := in
		in.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
		if err := rig.st.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	rig.h = Router(rig.deps) // a fresh cache: the rows were written past the bus
	out, _ := rig.overview(t)
	position := func(name string) int {
		for i, d := range out.Deployments {
			if d.Name == name {
				return i
			}
		}
		t.Fatalf("no deployment %s in %+v", name, out.Deployments)
		return -1
	}
	awake := slices.Index(out.States, "awake")
	var cards []string
	for _, node := range out.Nodes {
		if node.UUID != "gpu-d" {
			continue
		}
		for _, card := range node.Cards {
			cards = append(cards, fmt.Sprintf("%d state=%d awake=%d sleeping=%d occupants=%v", card.Index, card.State, card.AwakePct, card.SleepingPct, card.Occupants))
		}
	}
	w, s := position("wide")*2, position("side")*2+1
	want := []string{
		fmt.Sprintf("0 state=%d awake=50 sleeping=0 occupants=[%d]", awake, w),
		fmt.Sprintf("1 state=%d awake=50 sleeping=4 occupants=[%d %d]", awake, w, s),
	}
	if !slices.Equal(cards, want) {
		t.Fatalf("gpu-d cards = %v\nwant %v", cards, want)
	}
	if out.Summary.Cards != before.Summary.Cards+2 || out.Summary.Awake != before.Summary.Awake+2 || out.Summary.Asleep != before.Summary.Asleep {
		t.Fatalf("summary = %+v, before %+v: want two more cards, both awake", out.Summary, before.Summary)
	}
}
