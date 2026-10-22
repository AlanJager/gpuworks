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
	"gpuworks.ai/gpuworks/internal/server/store"
)

func instanceRequest(h http.HandlerFunc, uuid, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/"+query, nil)
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("uuid", uuid)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// replicaFixture adds to the PD fixture's nodes an ordinary two-replica
// deployment and a multi-node deployment of one leader and one follower.
func replicaFixture(t *testing.T) (*apiHandlers, *store.Store, *store.Deployment, *store.Deployment) {
	t.Helper()
	h, st, _ := createPDForOps(t)
	nodeA, _ := st.NodeByUUID("pd-a")
	nodeB, _ := st.NodeByUUID("pd-b")
	plain, err := st.CreateDeployment(&store.Deployment{Name: "plain", ServedName: "plain", State: "active", NodeID: nodeA.ID, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	multi, err := st.CreateDeployment(&store.Deployment{Name: "multi", ServedName: "multi", State: "active", NodeID: nodeA.ID, NodeIDs: fmt.Sprintf("[%d,%d]", nodeA.ID, nodeB.ID), NNodes: 2, Replicas: 1, Placement: `{"tp":2,"pp":1}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []store.Instance{
		{UUID: "plain-0", DeploymentID: plain.ID, NodeID: nodeA.ID, State: proto.InstRunning},
		{UUID: "plain-1", DeploymentID: plain.ID, NodeID: nodeA.ID, State: proto.InstRunning},
		{UUID: "plain-failed", DeploymentID: plain.ID, NodeID: nodeA.ID, State: proto.InstFailed},
		{UUID: "multi-leader", DeploymentID: multi.ID, NodeID: nodeA.ID, State: proto.InstRunning, NNodes: 2, DistRole: "leader", DistGroup: "g", DistAttempt: 1},
		{UUID: "multi-follower", DeploymentID: multi.ID, NodeID: nodeB.ID, State: proto.InstRunning, NNodes: 2, NodeRank: 1, Headless: true, DistRole: "follower", DistGroup: "g", DistAttempt: 1},
	} {
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	return h, st, plain, multi
}

func instanceStates(t *testing.T, st *store.Store, uuids ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, uuid := range uuids {
		inst, err := st.InstanceByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		out[uuid] = inst.State
	}
	return out
}

func TestRestartInstanceDrainsItsReplicaOnly(t *testing.T) {
	h, st, plain, _ := replicaFixture(t)
	rec := instanceRequest(h.restartInstance, "plain-1", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body.String())
	}
	if got := instanceStates(t, st, "plain-0", "plain-1", "plain-failed"); got["plain-1"] != proto.InstDraining || got["plain-0"] != proto.InstRunning || got["plain-failed"] != proto.InstFailed {
		t.Fatalf("states = %v", got)
	}
	event, found, _ := st.LatestScheduleEvent(plain.ID)
	if !found || event.Code != store.EventReplicaRestarted || !strings.Contains(event.Data, `"instance":"plain-1"`) || !strings.Contains(event.Data, `"drain":true`) {
		t.Fatalf("event = %+v", event)
	}
	var out struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID != plain.ID {
		t.Fatalf("body = %s", rec.Body.String())
	}
	// The same instance again: it no longer fills its replica.
	if rec := instanceRequest(h.restartInstance, "plain-1", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorInstanceNotServing) {
		t.Fatalf("second restart = %d %s", rec.Code, rec.Body.String())
	}
	if rec := instanceRequest(h.restartInstance, "plain-0", "?drain=false"); rec.Code != http.StatusAccepted {
		t.Fatalf("restart without drain = %d %s", rec.Code, rec.Body.String())
	}
	if got := instanceStates(t, st, "plain-0"); got["plain-0"] != proto.InstRemoving {
		t.Fatalf("without drain = %v", got)
	}
}

// A follower names its whole generation: draining keeps the follower with
// its leader, removing takes both.
func TestRestartInstanceThroughAFollowerActsOnTheGeneration(t *testing.T) {
	h, st, _, multi := replicaFixture(t)
	if rec := instanceRequest(h.restartInstance, "multi-follower", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body.String())
	}
	if got := instanceStates(t, st, "multi-leader", "multi-follower"); got["multi-leader"] != proto.InstDraining || got["multi-follower"] != proto.InstRunning {
		t.Fatalf("drain via follower = %v", got)
	}
	if event, _, _ := st.LatestScheduleEvent(multi.ID); !strings.Contains(event.Data, `"instance":"multi-leader"`) {
		t.Fatalf("the event names %s, want the leader", event.Data)
	}
	if err := st.SetInstanceState("multi-leader", proto.InstRunning, ""); err != nil {
		t.Fatal(err)
	}
	if rec := instanceRequest(h.restartInstance, "multi-follower", "?drain=false"); rec.Code != http.StatusAccepted {
		t.Fatalf("restart without drain = %d %s", rec.Code, rec.Body.String())
	}
	if got := instanceStates(t, st, "multi-leader", "multi-follower"); got["multi-leader"] != proto.InstRemoving || got["multi-follower"] != proto.InstRemoving {
		t.Fatalf("remove via follower = %v", got)
	}
}

func TestRestartInstanceRefusals(t *testing.T) {
	h, st, plain, _ := replicaFixture(t)
	cases := []struct {
		name, uuid, query string
		status            int
		code              string
	}{
		{name: "unknown instance", uuid: "nope", status: http.StatusNotFound},
		{name: "unreadable drain", uuid: "plain-0", query: "?drain=maybe", status: http.StatusBadRequest},
		{name: "failed attempt", uuid: "plain-failed", status: http.StatusConflict, code: ErrorInstanceNotServing},
	}
	for _, tc := range cases {
		rec := instanceRequest(h.restartInstance, tc.uuid, tc.query)
		if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if err := st.SetDeploymentState(plain.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if rec := instanceRequest(h.restartInstance, "plain-0", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorDeploymentNotActive) {
		t.Fatalf("stopped = %d %s", rec.Code, rec.Body.String())
	}
	if got := instanceStates(t, st, "plain-0"); got["plain-0"] != proto.InstRunning {
		t.Fatalf("a refused restart changed the instance: %v", got)
	}
}
