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
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// autoRig is the real router over a store with four online two-card H100
// nodes (au-1..au-4) on one management network, and a model. Requests go
// through the router with an admin session, so routing, auth and the
// handler are one path.
type autoRig struct {
	*tokenRig
	modelID uint
}

func newAutoRig(t *testing.T, nodes int) *autoRig {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	model, err := rig.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto", Revision: "main", Name: "org/auto", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= nodes; i++ {
		uuid := fmt.Sprintf("au-%d", i)
		launchConfigNode(t, rig.st, uuid)
		if err := rig.st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
		if err := rig.st.SetNodeState(uuid, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &autoRig{tokenRig: rig, modelID: model.ID}
}

// multiBody is a create/evaluate body for an automatic two-rank deployment
// with tp 2 (one card on each rank's node); extra is spliced in after the
// fixed fields.
func (r *autoRig) multiBody(name, extra string) string {
	return fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"nnodes":2,"placement_mode":"auto","placement":{"tp":2}%s}`, name, name, r.modelID, extra)
}

// pdAutoBody is an automatic PD body with the given prefill and decode units.
func (r *autoRig) pdAutoBody(name, prefill, decode, extra string) string {
	return fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"placement_mode":"auto"%s,"placement":{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[%s]},"decode":{"tp":1,"units":[%s]}}}}}`,
		name, name, r.modelID, extra, prefill, decode)
}

func (r *autoRig) create(t *testing.T, body string) (*httptest.ResponseRecorder, dto.Deployment) {
	t.Helper()
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", body)
	var out dto.Deployment
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec, out
}

func (r *autoRig) mustCreate(t *testing.T, body string) dto.Deployment {
	t.Helper()
	rec, out := r.create(t, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	return out
}

// occupy puts a running instance on node's cards so the node is busy for
// automatic placement.
func (r *autoRig) occupy(t *testing.T, node string, cards ...int) {
	t.Helper()
	n, err := r.st.NodeByUUID(node)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := r.st.CreateDeployment(&store.Deployment{Name: "hold-" + node, ServedName: "hold-" + node, State: store.DeployActive, NodeID: n.ID, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateInstance(&store.Instance{UUID: "hold-" + node, DeploymentID: dep.ID, NodeID: n.ID, State: proto.InstRunning, GPUIndexes: jsonOf(t, cards)}); err != nil {
		t.Fatal(err)
	}
}

func (r *autoRig) deployments(t *testing.T) int {
	t.Helper()
	all, err := r.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// storedDeployment is the row behind a created deployment.
func (r *autoRig) storedDeployment(t *testing.T, id uint) *store.Deployment {
	t.Helper()
	dep, err := r.st.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

// An automatic two-rank request that names no node gets two distinct nodes
// on one network, binpack unless it says otherwise: with one card of au-3
// held, binpack takes au-3 (the fewest free cards that still fit) and spread
// takes the nodes with the most free ones.
func TestCreateAutoMultiNodePicksTheRanks(t *testing.T) {
	r := newAutoRig(t, 4)
	r.occupy(t, "au-3", 0)

	out := r.mustCreate(t, r.multiBody("packed", ""))
	if out.PlacementMode != "auto" || out.NNodes != 2 || out.PlacementStrategy != "binpack" {
		t.Fatalf("created = mode %q nnodes %d strategy %q", out.PlacementMode, out.NNodes, out.PlacementStrategy)
	}
	if !slices.Equal(out.NodeUUIDs, []string{"au-3", "au-1"}) || out.NodeUUID != "au-3" || out.MasterAddr != "10.0.0.4" {
		t.Fatalf("binpack picked %v leader %s master %s, want au-3 then au-1 led by au-3 (10.0.0.4)", out.NodeUUIDs, out.NodeUUID, out.MasterAddr)
	}
	if len(out.Units) != 1 || out.Units[0].NodeUUID != "au-3" || len(out.Units[0].Followers) != 1 || out.Units[0].Followers[0].NodeUUID != "au-1" {
		t.Fatalf("units = %+v", out.Units)
	}
	dep := r.storedDeployment(t, out.ID)
	if !dep.UnitsAuto || dep.NNodes != 2 || dep.PlacementStrategy != store.PlacementBinpack {
		t.Fatalf("stored = auto %v nnodes %d strategy %q", dep.UnitsAuto, dep.NNodes, dep.PlacementStrategy)
	}
	// The request word is not stored: the placement keeps the parallelism only.
	if strings.Contains(dep.Placement, "auto") {
		t.Fatalf("stored placement = %s", dep.Placement)
	}

	// The mode is on the wire of the deployment read as well.
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", out.ID), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"placement_mode":"auto"`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}

	spread := r.mustCreate(t, r.multiBody("spread", `,"placement_strategy":"spread"`))
	if spread.PlacementMode != "auto" || spread.PlacementStrategy != "spread" || !slices.Equal(spread.NodeUUIDs, []string{"au-1", "au-2"}) {
		t.Fatalf("spread = mode %q strategy %q nodes %v, want au-1 and au-2", spread.PlacementMode, spread.PlacementStrategy, spread.NodeUUIDs)
	}
}

// The nodes a request names take its first ranks; the platform picks the
// rest from the group they are in. Naming every rank leaves nothing to pick.
func TestCreateAutoMultiNodeKeepsTheNamedRanks(t *testing.T) {
	r := newAutoRig(t, 4)
	r.occupy(t, "au-3", 0)

	partial := r.mustCreate(t, r.multiBody("partial", `,"node_uuids":["au-4"]`))
	if partial.PlacementMode != "auto" || !slices.Equal(partial.NodeUUIDs, []string{"au-4", "au-3"}) || partial.NodeUUID != "au-4" {
		t.Fatalf("one named: nodes %v leader %s mode %q, want au-4 leading then binpack's au-3", partial.NodeUUIDs, partial.NodeUUID, partial.PlacementMode)
	}
	full := r.mustCreate(t, r.multiBody("full", `,"node_uuids":["au-4","au-2"]`))
	if full.PlacementMode != "auto" || !slices.Equal(full.NodeUUIDs, []string{"au-4", "au-2"}) {
		t.Fatalf("every rank named: nodes %v mode %q", full.NodeUUIDs, full.PlacementMode)
	}
	if !r.storedDeployment(t, full.ID).UnitsAuto {
		t.Fatal("a request that named every rank is still placed by the platform from then on")
	}
}

func TestCreateAutoMultiNodeRefusals(t *testing.T) {
	r := newAutoRig(t, 4)
	noTP := strings.Replace(r.multiBody("no-tp", ""), `,"placement":{"tp":2}`, "", 1)
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"no placement.tp", noTP, http.StatusBadRequest, ErrorAutoPlacementTPRequired},
		{"an empty placement", strings.Replace(r.multiBody("empty", ""), `"placement":{"tp":2}`, `"placement":{}`, 1), http.StatusBadRequest, ErrorAutoPlacementTPRequired},
		{"more nodes named than ranks", r.multiBody("many", `,"node_uuids":["au-1","au-2","au-3"]`), http.StatusBadRequest, ErrorDistributedNodeCount},
		{"a named node that does not exist", r.multiBody("ghost", `,"node_uuids":["no-such-node"]`), http.StatusConflict, ErrorUnitPlacementNone},
		{"an unknown strategy", r.multiBody("strategy", `,"placement_strategy":"random"`), http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", tc.body)
			code, _ := errorBody(t, rec.Body.String())
			if rec.Code != tc.status || (tc.code != "" && code != tc.code) {
				t.Fatalf("create = %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
	if n := r.deployments(t); n != 0 {
		t.Fatalf("refused creates left %d deployments", n)
	}
}

// With nothing to place on the request is a conflict, not a bad request, and
// the counts say how many nodes each check left out as JSON numbers.
func TestCreateAutoMultiNodeWithNoRoomSaysWhy(t *testing.T) {
	r := newAutoRig(t, 4)
	for _, uuid := range []string{"au-1", "au-2", "au-3"} {
		r.occupy(t, uuid, 0, 1)
	}
	if _, err := r.st.SetNodeScheduling("au-4", store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"nodes": float64(4), "busy": float64(3), "cordoned": float64(1), "offline": float64(0), "unmet": float64(0), "no_network": float64(0), "no_peer_memory": float64(0), "largest": float64(0)}
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("none", ""))
	code, params := errorBody(t, rec.Body.String())
	if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || fmt.Sprint(params) != fmt.Sprint(want) {
		t.Fatalf("create = %d %s, want 409 UNIT_PLACEMENT_NONE with %v", rec.Code, rec.Body, want)
	}

	if n := r.deployments(t); n != 3 { // the three holders
		t.Fatalf("a refused create left %d deployments, want only the 3 holders", n)
	}

	// Offline nodes are counted as offline.
	t.Run("offline", func(t *testing.T) {
		r := newAutoRig(t, 3)
		for _, uuid := range []string{"au-1", "au-2", "au-3"} {
			if err := r.st.SetNodeState(uuid, store.NodeStateOffline); err != nil {
				t.Fatal(err)
			}
		}
		rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("none", ""))
		code, params := errorBody(t, rec.Body.String())
		if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || params["offline"] != float64(3) || params["nodes"] != float64(3) || params["busy"] != float64(0) {
			t.Fatalf("all offline = %d %s", rec.Code, rec.Body)
		}
	})

	// One node with room is not enough for two ranks: the largest group is 1.
	t.Run("one free node", func(t *testing.T) {
		r := newAutoRig(t, 2)
		r.occupy(t, "au-2", 0, 1)
		rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.multiBody("one", ""))
		code, params := errorBody(t, rec.Body.String())
		if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || params["largest"] != float64(1) || params["busy"] != float64(1) {
			t.Fatalf("one free node = %d %s", rec.Code, rec.Body)
		}
	})
}

// A multi-node request that places itself is unchanged: it is stored as the
// operator's own, whether it says "manual" or says nothing, and a
// single-node deployment has no placement mode at all.
func TestCreateManualShapesKeepTheirMode(t *testing.T) {
	r := newAutoRig(t, 4)
	manualBody := func(name, mode string) string {
		return strings.Replace(r.multiBody(name, `,"node_uuids":["au-2","au-4"]`), `"placement_mode":"auto",`, mode, 1)
	}
	for name, mode := range map[string]string{"silent": ``, "explicit": `"placement_mode":"manual",`} {
		out := r.mustCreate(t, manualBody("manual-"+name, mode))
		if out.PlacementMode != "manual" || !slices.Equal(out.NodeUUIDs, []string{"au-2", "au-4"}) || r.storedDeployment(t, out.ID).UnitsAuto {
			t.Fatalf("%s manual create = mode %q nodes %v", name, out.PlacementMode, out.NodeUUIDs)
		}
	}
	// A manual multi-node request still needs its nodes.
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", strings.Replace(r.multiBody("bare", ""), `"placement_mode":"auto",`, ``, 1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a manual multi-node create without nodes = %d %s, want 400", rec.Code, rec.Body)
	}

	// Single node: the mode word is the old per-node one and the response has
	// no placement_mode key.
	single := fmt.Sprintf(`{"name":"single","served_name":"single","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096},"node_uuid":"au-1","placement_mode":"auto"}`, r.modelID)
	rec = r.do(r.session, http.MethodPost, "/api/v1/deployments", single)
	if rec.Code != http.StatusCreated {
		t.Fatalf("single-node auto create = %d %s", rec.Code, rec.Body)
	}
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &wire)
	if _, present := wire["placement_mode"]; present {
		t.Fatalf("a single-node deployment carries placement_mode: %s", wire["placement_mode"])
	}
	var created dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if r.storedDeployment(t, created.ID).UnitsAuto {
		t.Fatal("a single-node deployment is not placed by units")
	}
}
