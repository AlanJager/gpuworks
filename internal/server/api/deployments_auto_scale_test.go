package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (r *autoRig) scale(t *testing.T, id uint, body string) (*httptest.ResponseRecorder, dto.Deployment) {
	t.Helper()
	rec := r.do(r.session, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/replicas", id), body)
	var out dto.Deployment
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec, out
}

// replicaNodes is each replica's nodes in rank order.
func replicaNodes(out dto.Deployment) [][]string {
	var all [][]string
	for _, unit := range out.Units {
		nodes := []string{unit.NodeUUID}
		for _, f := range unit.Followers {
			nodes = append(nodes, f.NodeUUID)
		}
		all = append(all, nodes)
	}
	return all
}

// holdReplica puts a running instance of the deployment on node so the
// shrink can name it.
func (r *autoRig) holdReplica(t *testing.T, dep dto.Deployment, uuid, node string, rank int) {
	t.Helper()
	n, err := r.st.NodeByUUID(node)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: n.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 2, NodeRank: rank, Headless: rank > 0, DistGroup: "g-" + uuid, DistAttempt: 1}); err != nil {
		t.Fatal(err)
	}
}

// An automatic multi-node deployment grows by count: the new replica's
// nodes are picked beside the listed ones, and place_on names the first of
// them. The mode stays automatic.
func TestScaleAutoMultiNodeGrowsByCount(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("scaled", ""))
	if !slices.Equal(dep.NodeUUIDs, []string{"au-1", "au-2"}) {
		t.Fatalf("created on %v", dep.NodeUUIDs)
	}

	rec, out := r.scale(t, dep.ID, `{"replicas":2}`)
	if rec.Code != http.StatusOK || out.Replicas != 2 || out.PlacementMode != "auto" {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body)
	}
	nodes := replicaNodes(out)
	if len(nodes) != 2 || !slices.Equal(nodes[0], []string{"au-1", "au-2"}) {
		t.Fatalf("replicas = %v, want the first kept", nodes)
	}
	// Binpack fills the nodes already carrying a rank (one card free), but a
	// node leads one replica only, so au-2 leads this one.
	if !slices.Equal(nodes[1], []string{"au-2", "au-1"}) {
		t.Fatalf("second replica on %v, want au-2 leading over au-1", nodes[1])
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 2 || !stored.UnitsAuto {
		t.Fatalf("stored replicas %d auto %v", stored.Replicas, stored.UnitsAuto)
	}

	// A third one cannot share those nodes any more (their cards are in
	// use): place_on names where the first rank goes, the other rank is
	// picked in its group.
	rec, out = r.scale(t, dep.ID, `{"replicas":3,"place_on":["au-4"]}`)
	nodes = replicaNodes(out)
	if rec.Code != http.StatusOK || len(nodes) != 3 || nodes[2][0] != "au-4" || nodes[2][1] == "au-4" || nodes[2][1] == "" {
		t.Fatalf("place_on au-4 = %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(nodes[0], []string{"au-1", "au-2"}) || !slices.Equal(nodes[1], []string{"au-2", "au-1"}) {
		t.Fatalf("grow moved the existing replicas: %v", nodes)
	}

	// Both ranks of the next replica named.
	rec, out = r.scale(t, dep.ID, `{"replicas":4,"place_on":["au-3","au-4"]}`)
	if nodes = replicaNodes(out); rec.Code != http.StatusOK || len(nodes) != 4 || !slices.Equal(nodes[3], []string{"au-3", "au-4"}) {
		t.Fatalf("both ranks named = %d %s", rec.Code, rec.Body)
	}
}

func TestScaleAutoMultiNodeRefusals(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("scaled", ""))
	if _, err := r.st.SetNodeScheduling("au-3", store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetNodeState("au-4", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
		params     map[string]any
	}{
		{"more nodes than the new ranks", `{"replicas":2,"place_on":["au-1","au-2","au-3"]}`, http.StatusBadRequest, ErrorReplicasPlaceOnTooMany, map[string]any{"count": float64(3), "maximum": float64(2)}},
		{"nodes named for no new replica", `{"replicas":1,"place_on":["au-1"]}`, http.StatusBadRequest, ErrorReplicasPlaceOnTooMany, map[string]any{"count": float64(1), "maximum": float64(0)}},
		{"a cordoned node named", `{"replicas":2,"place_on":["au-3"]}`, http.StatusConflict, ErrorReplicasPlaceOnRefused, map[string]any{"node": "au-3"}},
		{"an offline node named", `{"replicas":2,"place_on":["au-4"]}`, http.StatusConflict, ErrorReplicasPlaceOnRefused, map[string]any{"node": "au-4"}},
		{"an unknown node named", `{"replicas":2,"place_on":["no-such-node"]}`, http.StatusConflict, ErrorReplicasPlaceOnRefused, nil},
		{"no replicas", `{"replicas":0}`, http.StatusBadRequest, ErrorReplicasTooFew, nil},
		{"no count", `{}`, http.StatusBadRequest, ErrorReplicasBodyShape, map[string]any{"want": "units"}},
		{"unknown field", `{"replicas":2,"force":true}`, http.StatusBadRequest, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := r.scale(t, dep.ID, tc.body)
			code, params := errorBody(t, rec.Body.String())
			if rec.Code != tc.status || (tc.code != "" && code != tc.code) {
				t.Fatalf("PUT %s = %d %s, want %d %s", tc.body, rec.Code, rec.Body, tc.status, tc.code)
			}
			for k, v := range tc.params {
				if params[k] != v {
					t.Fatalf("params = %v, want %s=%v", params, k, v)
				}
			}
			if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
				t.Fatalf("a refused scale changed replicas to %d", stored.Replicas)
			}
		})
	}
	// An unknown deployment is a 404.
	if rec, _ := r.scale(t, 9999, `{"replicas":2}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", rec.Code)
	}
}

// Nothing to put a new replica on is a conflict that counts what each check
// left out - not a refusal of a node the operator named.
func TestScaleAutoMultiNodeWithNoRoom(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("scaled", ""))
	// The first replica's cards stay free of other holders; fill every
	// remaining card.
	r.occupy(t, "au-1", 1)
	r.occupy(t, "au-2", 1)
	r.occupy(t, "au-3", 0, 1)
	r.occupy(t, "au-4", 0, 1)
	rec, _ := r.scale(t, dep.ID, `{"replicas":2}`)
	code, params := errorBody(t, rec.Body.String())
	if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || params["nodes"] != float64(4) || params["busy"] != float64(4) {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused grow", stored.Replicas)
	}
}

// Shrinking drops the replicas the named instances belong to first, then the
// newest.
func TestScaleAutoMultiNodeShrinks(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("scaled", ""))
	rec, out := r.scale(t, dep.ID, `{"replicas":2,"place_on":["au-3","au-4"]}`)
	if rec.Code != http.StatusOK || len(out.Units) != 2 {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body)
	}
	r.holdReplica(t, dep, "r0-leader", "au-1", 0)
	r.holdReplica(t, dep, "r0-follower", "au-2", 1)
	r.holdReplica(t, dep, "r1-leader", "au-3", 0)
	r.holdReplica(t, dep, "r1-follower", "au-4", 1)

	// Without a name the newest replica goes.
	rec, out = r.scale(t, dep.ID, `{"replicas":1}`)
	if nodes := replicaNodes(out); rec.Code != http.StatusOK || len(nodes) != 1 || !slices.Equal(nodes[0], []string{"au-1", "au-2"}) || out.PlacementMode != "auto" {
		t.Fatalf("shrink = %d %s, want the replica on au-1 and au-2 kept", rec.Code, rec.Body)
	}

	// Naming an instance of the oldest replica drops that one instead.
	if _, out = r.scale(t, dep.ID, `{"replicas":2,"place_on":["au-3","au-4"]}`); len(out.Units) != 2 {
		t.Fatalf("regrow = %+v", out.Units)
	}
	rec, out = r.scale(t, dep.ID, `{"replicas":1,"remove":["r0-follower"]}`)
	if nodes := replicaNodes(out); rec.Code != http.StatusOK || len(nodes) != 1 || !slices.Equal(nodes[0], []string{"au-3", "au-4"}) {
		t.Fatalf("shrink naming r0-follower = %d %s, want the replica on au-3 and au-4 kept", rec.Code, rec.Body)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 || !stored.UnitsAuto {
		t.Fatalf("stored replicas %d auto %v", stored.Replicas, stored.UnitsAuto)
	}
}

func TestScaleAutoMultiNodeShrinkRefusals(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("scaled", ""))
	if rec, out := r.scale(t, dep.ID, `{"replicas":2,"place_on":["au-3","au-4"]}`); rec.Code != http.StatusOK || len(out.Units) != 2 {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body)
	}
	r.holdReplica(t, dep, "r0-leader", "au-1", 0)
	r.holdReplica(t, dep, "r1-leader", "au-3", 0)
	r.occupy(t, "au-4", 1) // another deployment's instance
	for _, tc := range []struct{ name, body, code string }{
		{"an instance of another deployment", `{"replicas":1,"remove":["hold-au-4"]}`, ErrorReplicasRemoveInvalid},
		{"an unknown instance", `{"replicas":1,"remove":["no-such-instance"]}`, ErrorReplicasRemoveInvalid},
		{"more instances than replicas dropped", `{"replicas":1,"remove":["r0-leader","r1-leader"]}`, ErrorReplicasRemoveTooMany},
		{"remove while growing", `{"replicas":3,"remove":["r0-leader"]}`, ErrorReplicasRemoveTooMany},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := r.scale(t, dep.ID, tc.body)
			if code, _ := errorBody(t, rec.Body.String()); rec.Code != http.StatusBadRequest || code != tc.code {
				t.Fatalf("PUT %s = %d %s, want 400 %s", tc.body, rec.Code, rec.Body, tc.code)
			}
			if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 2 {
				t.Fatalf("a refused shrink changed replicas to %d", stored.Replicas)
			}
		})
	}
}

// A manual multi-node deployment still lists its replicas as units; the
// mode, not the shape of the body, decides - and switching the mode changes
// which body is taken.
func TestScaleManualMultiNodeStillTakesUnits(t *testing.T) {
	r := newAutoRig(t, 4)
	manual := r.mustCreate(t, strings.Replace(r.multiBody("manual", `,"node_uuids":["au-1","au-2"]`), `"placement_mode":"auto",`, ``, 1))
	if manual.PlacementMode != "manual" {
		t.Fatalf("mode = %q", manual.PlacementMode)
	}
	for _, body := range []string{`{"replicas":2}`, `{"replicas":2,"place_on":["au-3"]}`, `{"replicas":1,"remove":["x"]}`} {
		rec, _ := r.scale(t, manual.ID, body)
		code, params := errorBody(t, rec.Body.String())
		if rec.Code != http.StatusBadRequest || code != ErrorReplicasBodyShape || params["want"] != "units" {
			t.Fatalf("manual PUT %s = %d %s, want 400 REPLICAS_BODY_SHAPE want=units", body, rec.Code, rec.Body)
		}
	}
	rec, out := r.scale(t, manual.ID, `{"units":[{"node_uuid":"au-1","followers":[{"node_uuid":"au-2"}]},{"node_uuid":"au-3","followers":[{"node_uuid":"au-4"}]}]}`)
	if rec.Code != http.StatusOK || len(out.Units) != 2 || out.PlacementMode != "manual" {
		t.Fatalf("manual units = %d %s", rec.Code, rec.Body)
	}

	// The same deployment, switched to automatic, takes a count; back to
	// manual, units again.
	if rec := r.do(r.session, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/placement-mode", manual.ID), `{"mode":"auto"}`); rec.Code != http.StatusOK {
		t.Fatalf("switch = %d %s", rec.Code, rec.Body)
	}
	if rec, out = r.scale(t, manual.ID, `{"replicas":1}`); rec.Code != http.StatusOK || len(out.Units) != 1 || out.PlacementMode != "auto" {
		t.Fatalf("auto count = %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(r.session, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/placement-mode", manual.ID), `{"mode":"manual"}`); rec.Code != http.StatusOK {
		t.Fatalf("switch back = %d %s", rec.Code, rec.Body)
	}
	if rec, _ = r.scale(t, manual.ID, `{"replicas":2}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("manual again, count = %d %s", rec.Code, rec.Body)
	}
}

// The replicas route is a write: a read token and no token are refused
// before the body is looked at, and nothing changes.
func TestScaleAutoRouteAuth(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("scaled", ""))
	path := fmt.Sprintf("/api/v1/deployments/%d/replicas", dep.ID)
	if rec := r.do("", http.MethodPut, path, `{"replicas":2}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(r.mint("read"), http.MethodPut, path, `{"replicas":2}`); rec.Code != http.StatusForbidden {
		t.Fatalf("read token = %d %s", rec.Code, rec.Body)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
		t.Fatalf("a refused request changed replicas to %d", stored.Replicas)
	}
	if rec := r.do(r.mint("write"), http.MethodPut, path, `{"replicas":2}`); rec.Code != http.StatusOK {
		t.Fatalf("write token = %d %s", rec.Code, rec.Body)
	}
}
