package api

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentOfflineReadinessAndReconnect(t *testing.T) {
	f := newListFixture(t, 0)
	nodes, _ := f.st.ListNodes()
	for _, n := range nodes {
		if err := f.st.SetNodeState(n.UUID, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
	}
	check := func(state string, ready int) {
		t.Helper()
		page := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=100&q=scheduled-away"))
		if len(page.Items) != 1 {
			t.Fatalf("items = %+v", page.Items)
		}
		row := page.Items[0]
		if row.RuntimeState != state || row.ReadyReplicas == nil || *row.ReadyReplicas != ready {
			t.Fatalf("state/readiness = %s/%v, want %s/%d", row.RuntimeState, row.ReadyReplicas, state, ready)
		}
		detail := f.handler.deploymentDTO(mustDeployment(t, f.st, row.ID), true)
		if detail.RuntimeState != state || detail.ReadyReplicas == nil || *detail.ReadyReplicas != ready {
			t.Fatalf("detail = %+v", detail)
		}
	}
	check("running", 1)
	// Placement originally named list-a, but the actual replica runs on list-b.
	if err := f.st.SetNodeState("list-a", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	check("running", 1)
	if err := f.st.SetNodeState("list-b", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	check("offline", 0)
	instances, _ := f.st.ListInstances()
	for _, inst := range instances {
		if inst.UUID == "inst-scheduled-away" && inst.State != proto.InstRunning {
			t.Fatalf("read rewrote observation: %+v", inst)
		}
	}
	if err := f.st.SetNodeState("list-b", store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	check("running", 1)
}

func TestDeploymentDistributedOfflineFollower(t *testing.T) {
	f := newListFixture(t, 0)
	nodes, _ := f.st.ListNodes()
	for _, n := range nodes {
		_ = f.st.SetNodeState(n.UUID, store.NodeStateOnline)
	}
	a, _ := f.st.NodeByUUID("list-a")
	b, _ := f.st.NodeByUUID("list-b")
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "distributed", State: store.DeployActive, NodeID: a.ID, NodeIDs: fmt.Sprintf("[%d,%d]", a.ID, b.ID), NNodes: 2, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Instance{
		{UUID: "leader", NodeID: a.ID, State: proto.InstRunning, NNodes: 2, DistGroup: "group", DistAttempt: 1},
		{UUID: "follower", NodeID: b.ID, State: proto.InstRunning, Headless: true, NNodes: 2, NodeRank: 1, DistGroup: "group", DistAttempt: 1},
	} {
		inst.DeploymentID = dep.ID
		if _, err := f.st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	// The state a row shows follows the runtime state, except that a replica
	// asleep with its whole unit reads asleep; an offline follower outranks
	// sleep, since the deployment cannot serve for another reason.
	check := func(state, shown string, ready int) {
		t.Helper()
		got := f.handler.deploymentDTO(dep, true)
		if got.RuntimeState != state || got.State != shown || got.ReadyReplicas == nil || *got.ReadyReplicas != ready {
			t.Fatalf("got %s/%s/%d, want %s/%s/%d", got.RuntimeState, got.State, *got.ReadyReplicas, state, shown, ready)
		}
	}
	check("running", "running", 1)
	if err := f.st.SetNodeState("list-b", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	check("offline", "offline", 0)
	if err := f.st.SetNodeState("list-b", store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	check("running", "running", 1)
	if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", "leader").Update("sleep_level", 1).Error; err != nil {
		t.Fatal(err)
	}
	// The leader rules the unit: the follower never reports a level of its
	// own, and one sleeping rank does not make the unit half asleep.
	check("running", "asleep", 0)
	if err := f.st.SetNodeState("list-b", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	check("offline", "offline", 0)
	if err := f.st.SetNodeState("list-b", store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	check("running", "asleep", 0)
}

func TestDeploymentPDOfflineReadiness(t *testing.T) {
	h, st, created := createPDForOps(t)
	for _, rank := range []struct{ uuid, role, node, gpus string }{{"prefill", "prefill", "pd-a", "[0]"}, {"decode", "decode", "pd-b", "[0,1]"}} {
		n, _ := st.NodeByUUID(rank.node)
		if _, err := st.CreateInstance(&store.Instance{UUID: rank.uuid, DeploymentID: created.ID, NodeID: n.ID, State: proto.InstRunning, Role: rank.role, GPUIndexes: rank.gpus}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(ready int, state string) {
		t.Helper()
		got := h.deploymentDTO(mustDeployment(t, st, created.ID), true)
		total := 0
		for _, r := range got.PD.Roles {
			total += r.Ready
		}
		if total != ready || got.PD.State != state {
			t.Fatalf("PD = %+v", got.PD)
		}
	}
	check(2, "running")
	if err := st.SetNodeState("pd-b", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	check(1, "partial")
	if err := st.SetNodeState("pd-b", store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	check(2, "running")
}
