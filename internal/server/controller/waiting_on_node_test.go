package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
		// The re-check is the short poll, not the refusal record's window: an
		// eviction of another model on that node wakes nothing, so a replica
		// waiting for the eviction would otherwise wait a whole
		// PendingTimeout (600 s by default) to look again.
		c.q.mu.Lock()
		at, queued := c.q.pending[depKey(dep.ID)]
		c.q.mu.Unlock()
		if !queued {
			t.Fatal("the wait did not schedule its re-check")
		}
		if until := time.Until(at); until > groupPollInterval+2*time.Second {
			t.Fatalf("the re-check is due in %s, want at most %s", until, groupPollInterval)
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

// scheduleCodes lists the latest scheduling reason of each pass, with the
// clock stepped between passes so the throttle's windows elapse.
func scheduleCodes(t *testing.T, c *Controller, st *store.Store, dep *store.Deployment, steps ...time.Duration) []string {
	t.Helper()
	var codes []string
	for _, step := range steps {
		c.now = func() time.Time { return base.Add(step) }
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
		ev, found, err := st.LatestScheduleEvent(dep.ID)
		if err != nil || !found {
			t.Fatalf("no event after %s: found=%v err=%v", step, found, err)
		}
		var data struct {
			Scheduler scheduler.Reason `json:"scheduler"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
			t.Fatal(err)
		}
		codes = append(codes, data.Scheduler.Code+"/"+paramString(data.Scheduler, "stage"))
	}
	return codes
}

func paramString(r scheduler.Reason, key string) string { v, _ := r.Params[key].(string); return v }

var base = time.Unix(1_800_000_000, 0)

// An image still arriving keeps its own reason: it is not the model arriving,
// and the two alternating would defeat the exemption that keeps a live image
// pull from reading as a stall (GWKS-507 review).
func TestAnImageWaitIsNotRecordedAsTheModelArriving(t *testing.T) {
	c, st, dep, _, _, _, _ := imageCopyFixture(t, "wait_image_copy")
	codes := scheduleCodes(t, c, st, dep, 0, 11*time.Second)
	for _, code := range codes {
		if code != "image_delivery_waiting/" {
			t.Fatalf("an image wait was recorded as %v, want its own reason on every pass", codes)
		}
	}
}

// A node that goes offline is recorded, naming it; and it says the node is
// offline so the reconnect grace still suppresses it after a server restart.
func TestAnOfflineNodesWaitNamesIt(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "wait_offline", 2, 1)
	hub.online[nodes[0].UUID] = false
	ids, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(ids), ServedName: "offline", Engine: "vllm",
		State: "active", NNodes: 2, MasterAddr: "10.0.0.1", MasterPort: 29500, Placement: `{"tp":2}`})
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, hub, nil)
	c.now = func() time.Time { return base }
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	ev, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found {
		t.Fatalf("no event: found=%v err=%v", found, err)
	}
	for _, want := range []string{`"code":"waiting_on_node"`, `"stage":"node_offline"`, `"node":"` + nodes[0].DisplayName() + `"`} {
		if !strings.Contains(ev.Data, want) {
			t.Fatalf("event %s lacks %s", ev.Data, want)
		}
	}
}

// A different stage is news: once the model is there and an eviction holds
// the node, the wait says the eviction after the change floor, not after a
// whole pending-timeout window.
func TestAChangedStageIsRecordedAsNews(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "wait_stage_change", 1, 4)
	if err := st.DB().Model(&store.ModelFile{}).Where("model_id = ?", model.ID).Update("state", proto.MFPending).Error; err != nil {
		t.Fatal(err)
	}
	c := New(st, hub, nil)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	if codes := scheduleCodes(t, c, st, dep, 0); codes[0] != "waiting_on_node/model_arriving" {
		t.Fatalf("first pass %v", codes)
	}
	if err := st.DB().Model(&store.ModelFile{}).Where("model_id = ?", model.ID).Update("state", proto.MFPresent).Error; err != nil {
		t.Fatal(err)
	}
	release, ok := st.TryEvictNodeCache(nodes[0].ID)
	if !ok {
		t.Fatal("could not reserve the node")
	}
	defer release()
	if codes := scheduleCodes(t, c, st, dep, 11*time.Second); codes[0] != "waiting_on_node/cache_eviction" {
		t.Fatalf("after the change floor: %v", codes)
	}
}
