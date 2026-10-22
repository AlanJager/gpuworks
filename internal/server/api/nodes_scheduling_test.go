package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// schedulingFixture is two nodes and, on node "drain-a", a replica of each
// kind a drain treats differently: a pool replica, one of a deployment pinned
// to the node, and the follower rank of a multi-node replica whose leader runs
// on "drain-b".
func schedulingFixture(t *testing.T) (*apiHandlers, *store.Store, map[string]*store.Deployment) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/drain", Revision: "main", Name: "drain"})
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]*store.Node{}
	for _, uuid := range []string{"drain-a", "drain-b"} {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		nodes[uuid] = node
	}
	a, b := nodes["drain-a"], nodes["drain-b"]
	deps := map[string]*store.Deployment{}
	create := func(name string, d store.Deployment, reqs map[string][]string) *store.Deployment {
		d.Name, d.ModelID, d.Engine, d.State = name, model.ID, "vllm", store.DeployActive
		dep, err := st.CreateDeploymentWithRequirements(&d, reqs)
		if err != nil {
			t.Fatal(err)
		}
		deps[name] = dep
		return dep
	}
	instance := func(uuid string, dep *store.Deployment, node *store.Node, extra store.Instance) {
		extra.UUID, extra.DeploymentID, extra.NodeID = uuid, dep.ID, node.ID
		if extra.State == "" {
			extra.State = proto.InstRunning
		}
		if _, err := st.CreateInstance(&extra); err != nil {
			t.Fatal(err)
		}
	}
	pool := create("pool", store.Deployment{NodeID: a.ID, Replicas: 1, Placement: `{"tp":1}`}, nil)
	instance("pool-1", pool, a, store.Instance{})
	pinned := create("pinned", store.Deployment{NodeID: a.ID, Replicas: 1, Placement: `{"tp":1}`}, map[string][]string{"gpuworks.ai/node": {"drain-a"}})
	instance("pinned-1", pinned, a, store.Instance{})
	nodeIDs, _ := json.Marshal([]uint{b.ID, a.ID})
	multi := create("multi", store.Deployment{NodeID: b.ID, NodeIDs: string(nodeIDs), NNodes: 2, Replicas: 1, DistGroup: "g-multi"}, nil)
	instance("multi-leader", multi, b, store.Instance{NNodes: 2, DistGroup: "g-multi", DistAttempt: 1,
		ScheduleExpl: planning.NewScheduleRecord(planning.LaunchPlan{}, false, nil, &planning.UnitRecord{Followers: "drain-a[]"}).Encode()})
	instance("multi-follower", multi, a, store.Instance{NNodes: 2, NodeRank: 1, Headless: true, DistGroup: "g-multi", DistAttempt: 1})
	instance("pool-old", pool, a, store.Instance{State: proto.InstStopped})
	return &apiHandlers{d: Deps{Store: st}}, st, deps
}

func callNode(t *testing.T, handler http.HandlerFunc, method, uuid, path, body string) (int, string) {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("uuid", uuid)
	req := httptest.NewRequest(method, "/api/v1/nodes/"+uuid+path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec.Code, rec.Body.String()
}

// A node is cordoned, drained and made schedulable again by name; the node
// reads back its state, and anything else is refused.
func TestSetNodeSchedulingWire(t *testing.T) {
	h, st, _ := schedulingFixture(t)
	for _, want := range []string{"cordoned", "draining", "schedulable"} {
		status, body := callNode(t, h.setNodeScheduling, http.MethodPut, "drain-a", "/scheduling", `{"scheduling":"`+want+`"}`)
		if status != http.StatusOK || !strings.Contains(body, `"uuid":"drain-a","scheduling":"`+want+`"`) {
			t.Fatalf("%s = %d %s", want, status, body)
		}
	}
	if node, _ := st.NodeByUUID("drain-a"); node.Scheduling != store.NodeSchedulable {
		t.Fatalf("stored %q after schedulable", node.Scheduling)
	}
	for name, c := range map[string]struct {
		body   string
		status int
		want   string
	}{
		"an unknown state":      {`{"scheduling":"paused"}`, http.StatusBadRequest, ErrorFieldInvalid},
		"the stored zero value": {`{"scheduling":""}`, http.StatusBadRequest, ErrorFieldInvalid},
		"an unknown field":      {`{"scheduling":"cordoned","force":true}`, http.StatusBadRequest, "unknown field"},
		"not an object":         {`[]`, http.StatusBadRequest, "cannot unmarshal"},
	} {
		if status, body := callNode(t, h.setNodeScheduling, http.MethodPut, "drain-a", "/scheduling", c.body); status != c.status || !strings.Contains(body, c.want) {
			t.Errorf("%s = %d %s", name, status, body)
		}
	}
	if status, _ := callNode(t, h.setNodeScheduling, http.MethodPut, "missing", "/scheduling", `{"scheduling":"cordoned"}`); status != http.StatusNotFound {
		t.Errorf("unknown node = %d", status)
	}
	rec := httptest.NewRecorder()
	Router(Deps{Store: st}).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/nodes/drain-a/scheduling", strings.NewReader(`{"scheduling":"cordoned"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d", rec.Code)
	}
}

// What still runs on a node, and whether a drain moves it: the pool replica
// is moved, the pinned one waits, the multi-node rank names its node.
// Stopped rows are history, not replicas.
func TestNodeReplicasSayWhatADrainMoves(t *testing.T) {
	h, _, deps := schedulingFixture(t)
	status, body := callNode(t, h.nodeReplicas, http.MethodGet, "drain-a", "/replicas", "")
	want := fmt.Sprintf(`{"scheduling":"schedulable","items":[`+
		`{"instance":"pool-1","deployment_id":%d,"deployment":"pool","state":"running","kind":"pool"},`+
		`{"instance":"pinned-1","deployment_id":%d,"deployment":"pinned","state":"running","kind":"pinned"},`+
		`{"instance":"multi-follower","deployment_id":%d,"deployment":"multi","state":"running","kind":"named"}]}`,
		deps["pool"].ID, deps["pinned"].ID, deps["multi"].ID)
	if status != http.StatusOK || strings.TrimSpace(body) != want {
		t.Fatalf("replicas = %d %s\nwant %s", status, body, want)
	}
	if status, _ := callNode(t, h.nodeReplicas, http.MethodGet, "missing", "/replicas", ""); status != http.StatusNotFound {
		t.Errorf("unknown node = %d", status)
	}
}

// Evicting takes every replica off a cordoned node at once: each generation
// with an instance there drains its leader - for the multi-node replica, the
// leader on the other node - and costs no retry. A schedulable node is
// refused.
func TestEvictNodeDrainsEveryGenerationOnIt(t *testing.T) {
	h, st, _ := schedulingFixture(t)
	if status, body := callNode(t, h.evictNode, http.MethodPost, "drain-a", "/evict", ""); status != http.StatusConflict || !strings.Contains(body, ErrorNodeSchedulable) {
		t.Fatalf("evicting a schedulable node = %d %s", status, body)
	}
	if _, err := st.SetNodeScheduling("drain-a", store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	status, body := callNode(t, h.evictNode, http.MethodPost, "drain-a", "/evict", "")
	if status != http.StatusAccepted || strings.TrimSpace(body) != `{"evicted":["multi-leader","pinned-1","pool-1"]}` {
		t.Fatalf("evict = %d %s", status, body)
	}
	for _, uuid := range []string{"pool-1", "pinned-1", "multi-leader"} {
		inst, _ := st.InstanceByUUID(uuid)
		if inst.State != proto.InstDraining || !strings.Contains(inst.Message, "evicted from node drain-a") {
			t.Errorf("%s = %s %q", uuid, inst.State, inst.Message)
		}
	}
	if follower, _ := st.InstanceByUUID("multi-follower"); follower.State != proto.InstRunning {
		t.Errorf("the follower was touched directly: %s (its leader's drain takes it)", follower.State)
	}
	// Nothing left that fills a replica: a second eviction evicts nothing.
	if status, body := callNode(t, h.evictNode, http.MethodPost, "drain-a", "/evict", ""); status != http.StatusAccepted || strings.TrimSpace(body) != `{"evicted":[]}` {
		t.Fatalf("second evict = %d %s", status, body)
	}

	if status, _ := callNode(t, h.evictNode, http.MethodPost, "missing", "/evict", ""); status != http.StatusNotFound {
		t.Errorf("unknown node = %d", status)
	}
}

// The deploy form never offers a cordoned node: it is ineligible with its
// reason, and automatic placement selects among the others.
func TestEvaluateSkipsACordonedNode(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	if _, err := st.SetNodeScheduling("auto-big", store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID))))
	var got struct {
		SelectedNode *struct {
			UUID string `json:"uuid"`
		} `json:"selected_node"`
		EligibleNodes []struct {
			UUID     string `json:"uuid"`
			Eligible bool   `json:"eligible"`
			Reasons  []struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			} `json:"reasons"`
		} `json:"eligible_nodes"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	if got.SelectedNode == nil || got.SelectedNode.UUID != "auto-labelled" {
		t.Fatalf("selected %+v, want auto-labelled: auto-big is cordoned", got.SelectedNode)
	}
	for _, node := range got.EligibleNodes {
		if node.UUID != "auto-big" {
			continue
		}
		if node.Eligible || len(node.Reasons) == 0 || node.Reasons[0].Code != "node_cordoned" || node.Reasons[0].Params["node"] != "auto-big" {
			t.Fatalf("auto-big = %+v", node)
		}
		return
	}
	t.Fatal("auto-big missing from eligible_nodes")
}
