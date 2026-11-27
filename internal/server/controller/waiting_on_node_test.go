package controller

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// waitingStage is the stage of the deployment's latest scheduling record when
// it is a waiting_on_node one, and whether the pass wrote an instance.
func waitingStage(t *testing.T, c *Controller, st *store.Store, dep *store.Deployment) (stage string, node any, instances int) {
	t.Helper()
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	list, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return "", nil, len(list)
	}
	var data struct {
		Scheduler scheduler.Reason `json:"scheduler"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
		t.Fatal(err)
	}
	if data.Scheduler.Code != scheduler.ReasonWaitingOnNode {
		t.Fatalf("latest scheduling record is %s, want %s: %s", data.Scheduler.Code, scheduler.ReasonWaitingOnNode, ev.Data)
	}
	stage, _ = data.Scheduler.Params["stage"].(string)
	return stage, data.Scheduler.Params["node"], len(list)
}

// A replica whose node was chosen but cannot start there yet used to end the
// pass with no instance, no record and so no reason or stall, ever: the
// deployment read as pending forever (GWKS-507). Each such wait is recorded
// now, naming the node and what it waits for, and it schedules the re-check.
func TestAPlacedReplicaThatCannotStartSaysWhatItWaitsFor(t *testing.T) {
	t.Run("a cache eviction owns the node", func(t *testing.T) {
		st, hub, nodes, model := poolFixture(t, "wait_evict", 1, 16)
		c := New(st, hub, nil)
		dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
		release, ok := st.TryEvictNodeCache(nodes[0].ID)
		if !ok {
			t.Fatal("could not reserve the node")
		}
		stage, node, insts := waitingStage(t, c, st, dep)
		if insts != 0 || stage != waitingStageCacheEviction || node != nodes[0].DisplayName() {
			t.Fatalf("instances=%d stage=%q node=%v", insts, stage, node)
		}
		c.q.mu.Lock()
		_, queued := c.q.pending[depKey(dep.ID)]
		c.q.mu.Unlock()
		if !queued {
			t.Fatal("the wait did not schedule its re-check")
		}
		release()
		if _, _, insts := waitingStage(t, c, st, dep); insts != 1 {
			t.Fatalf("the replica did not start once the eviction ended: %d instances", insts)
		}
	})

	t.Run("the model is still arriving on the node", func(t *testing.T) {
		st, hub, nodes, model := poolFixture(t, "wait_model", 1, 16)
		if err := st.DB().Model(&store.ModelFile{}).Where("model_id = ?", model.ID).Update("state", proto.MFPending).Error; err != nil {
			t.Fatal(err)
		}
		c := New(st, hub, nil)
		dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
		stage, node, insts := waitingStage(t, c, st, dep)
		if insts != 0 || stage != waitingStageModelArriving || node != nodes[0].DisplayName() {
			t.Fatalf("instances=%d stage=%q node=%v", insts, stage, node)
		}
	})

}

// Two replicas on a 16-card node whose first two cards are held outside the
// platform both start: held cards are not a reason to place one at a time.
func TestHeldCardsDoNotHoldBackReplicasThatFitOnTheFreeOnes(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "held_two", 1, 16)
	fresh, err := st.NodeWithGPUs(nodes[0].UUID)
	if err != nil {
		t.Fatal(err)
	}
	held, err := json.Marshal([]proto.CardProcess{{PID: 1, Name: "python3", MemoryBytes: 87 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range fresh.GPUs {
		if g.Index < 2 {
			if err := st.DB().Model(&store.GPU{}).Where("id = ?", g.ID).Update("foreign_processes", string(held)).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	c := New(st, hub, nil)
	deps := []*store.Deployment{
		poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil),
	}
	second, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/held_two_b", Revision: "main", Name: "held_two_b", WeightBytes: 1 << 30, Meta: poolModelMeta})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: second.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: "/models/held_two_b"}); err != nil {
		t.Fatal(err)
	}
	deps = append(deps, poolDeployment(t, st, second, nodes[0].ID, 1, store.PlacementSpread, nil))
	for _, dep := range deps {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
		list, err := st.InstancesForDeployment(dep.ID)
		if err != nil || len(list) != 1 {
			t.Fatalf("deployment %d: %d instances, err=%v", dep.ID, len(list), err)
		}
	}
}
