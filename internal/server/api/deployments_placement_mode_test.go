package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (r *autoRig) updatePD(id uint, body string) (int, string, dto.Deployment) {
	rec := r.do(r.session, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/pd", id), body)
	var out dto.Deployment
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, rec.Body.String(), out
}

// A PD update on an automatic deployment may add units without a node: they
// are placed beside the units that are, and the cards the placed units take
// are counted. The deployment stays automatic and pins no card.
func TestUpdateAutoPDPlacesNewUnits(t *testing.T) {
	r := newAutoRig(t, 3)
	r.occupy(t, "au-1", 0, 1)
	pd := r.mustCreate(t, r.pdAutoBody("pd-grow", `{}`, `{}`, ""))
	prefill := pdNodes(t, pd, "prefill")
	if !slices.Equal(prefill, []string{"au-2"}) || !slices.Equal(pdNodes(t, pd, "decode"), []string{"au-2"}) {
		t.Fatalf("created on prefill %v decode %v", prefill, pdNodes(t, pd, "decode"))
	}

	// au-2's two cards are taken by the placed units: the new prefill unit
	// goes to au-3, not onto au-2.
	status, body, out := r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"au-2"},{}]}}}`)
	if status != http.StatusOK || out.PlacementMode != "auto" {
		t.Fatalf("update = %d %s", status, body)
	}
	if got := pdNodes(t, out, "prefill"); !slices.Equal(got, []string{"au-2", "au-3"}) {
		t.Fatalf("prefill on %v, want the placed unit kept and the new one on au-3", got)
	}
	if got := pdNodes(t, out, "decode"); !slices.Equal(got, []string{"au-2"}) {
		t.Fatalf("decode moved to %v", got)
	}
	stored := r.storedDeployment(t, pd.ID)
	if !stored.UnitsAuto || strings.Contains(stored.Placement, `"gpus"`) || strings.Contains(stored.Placement, `"node_uuid":""`) {
		t.Fatalf("stored auto %v placement %s", stored.UnitsAuto, stored.Placement)
	}

	// A named node is the new unit's own placement.
	status, body, out = r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"au-2"},{"node_uuid":"au-3"},{"node_uuid":"au-3"}]}}}`)
	if status != http.StatusOK || !slices.Equal(pdNodes(t, out, "prefill"), []string{"au-2", "au-3", "au-3"}) {
		t.Fatalf("named update = %d %s", status, body)
	}
}

func TestUpdateAutoPDRefusals(t *testing.T) {
	r := newAutoRig(t, 2)
	pd := r.mustCreate(t, r.pdAutoBody("pd-grow", `{}`, `{}`, ""))
	before := r.storedDeployment(t, pd.ID).Placement

	status, body, _ := r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"au-1"},{"followers":[{"node_uuid":"au-2"}]}]}}}`)
	if code, params := errorBody(t, body); status != http.StatusBadRequest || code != ErrorAutoPlacementPDMultiNode || params["role"] != "prefill" {
		t.Fatalf("a unit with followers = %d %s", status, body)
	}

	// Every remaining card is held: the new unit has nowhere to go.
	r.occupy(t, "au-1", 0, 1)
	r.occupy(t, "au-2", 0, 1)
	status, body, _ = r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"au-1"},{}]}}}`)
	if code, params := errorBody(t, body); status != http.StatusConflict || code != ErrorUnitPlacementNone || params["nodes"] != float64(2) {
		t.Fatalf("no room = %d %s", status, body)
	}
	if after := r.storedDeployment(t, pd.ID).Placement; after != before {
		t.Fatalf("a refused update changed the placement:\n%s\n%s", before, after)
	}
}

// A manual PD deployment still needs its units to name their nodes.
func TestUpdateManualPDStillNeedsNodes(t *testing.T) {
	r := newAutoRig(t, 2)
	body := strings.Replace(r.pdAutoBody("pd-man", `{"node_uuid":"au-1"}`, `{"node_uuid":"au-2"}`, ""), `"placement_mode":"auto",`, ``, 1)
	pd := r.mustCreate(t, body)
	if pd.PlacementMode != "manual" {
		t.Fatalf("mode = %q", pd.PlacementMode)
	}
	status, out, _ := r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"au-1"},{}]}}}`)
	if code, _ := errorBody(t, out); status != http.StatusBadRequest || code != messages.ErrorPDNodeRequired {
		t.Fatalf("a manual unit without a node = %d %s, want 400 %s", status, out, messages.ErrorPDNodeRequired)
	}
}

func (r *autoRig) setMode(id uint, body string) *httptest.ResponseRecorder {
	return r.do(r.session, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d/placement-mode", id), body)
}

func TestPlacementModeRouteAuthAndTarget(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("switch", ""))
	path := fmt.Sprintf("/api/v1/deployments/%d/placement-mode", dep.ID)
	const body = `{"mode":"manual"}`

	if rec := r.do("", http.MethodPut, path, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s, want 401", rec.Code, rec.Body)
	}
	read := r.mint("read")
	if rec := r.do(read, http.MethodPut, path, body); rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
		t.Fatalf("read token = %d %s, want 403 PLATFORM_TOKEN_READ_ONLY", rec.Code, rec.Body)
	}
	if !r.storedDeployment(t, dep.ID).UnitsAuto {
		t.Fatal("a refused request changed the mode")
	}
	rec := r.do(r.mint("write"), http.MethodPut, path, body)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["placement_mode"] != "manual" {
		t.Fatalf("write token = %d %s", rec.Code, rec.Body)
	}
	if rec := r.setMode(9999, body); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d %s, want 404", rec.Code, rec.Body)
	}
	// Routing answers before the scope check, for either token.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		for _, token := range []string{r.session, read} {
			if rec := r.do(token, method, path, "{}"); rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s = %d %s, want 405", method, rec.Code, rec.Body)
			}
		}
	}
}

func TestPlacementModeSwitchesAMultiNodeDeployment(t *testing.T) {
	r := newAutoRig(t, 4)
	dep := r.mustCreate(t, r.multiBody("switch", ""))

	rec := r.setMode(dep.ID, `{"mode":"manual"}`)
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.PlacementMode != "manual" || out.ID != dep.ID || !slices.Equal(out.NodeUUIDs, dep.NodeUUIDs) || len(out.Units) != 1 {
		t.Fatalf("to manual = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"placement_mode":"manual"`) || r.storedDeployment(t, dep.ID).UnitsAuto {
		t.Fatalf("manual not stored: %s", rec.Body)
	}
	// The setting is what the deployment reads back as.
	if got := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", dep.ID), ""); !strings.Contains(got.Body.String(), `"placement_mode":"manual"`) {
		t.Fatalf("GET = %s", got.Body)
	}

	// Switching to the mode it is in is accepted and changes nothing.
	rec = r.setMode(dep.ID, `{"mode":"manual"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"placement_mode":"manual"`) {
		t.Fatalf("manual again = %d %s", rec.Code, rec.Body)
	}
	rec = r.setMode(dep.ID, `{"mode":"auto"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.PlacementMode != "auto" || !slices.Equal(out.NodeUUIDs, dep.NodeUUIDs) || !r.storedDeployment(t, dep.ID).UnitsAuto {
		t.Fatalf("to auto = %d %s", rec.Code, rec.Body)
	}
	if n := r.storedDeployment(t, dep.ID).Replicas; n != 1 {
		t.Fatalf("switching restarted or rescaled the deployment: replicas %d", n)
	}
}

func TestPlacementModeRefusals(t *testing.T) {
	r := newAutoRig(t, 2)
	multi := r.mustCreate(t, r.multiBody("multi", ""))
	single, err := r.st.CreateDeployment(&store.Deployment{Name: "single", ServedName: "single", ModelID: r.modelID, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		id         uint
		status     int
		code       string
	}{
		{"an unknown mode", `{"mode":"sometimes"}`, multi.ID, http.StatusBadRequest, ErrorFieldInvalid},
		{"no mode", `{}`, multi.ID, http.StatusBadRequest, ErrorFieldInvalid},
		{"an empty mode", `{"mode":""}`, multi.ID, http.StatusBadRequest, ErrorFieldInvalid},
		{"an unknown field", `{"mode":"auto","force":true}`, multi.ID, http.StatusBadRequest, ""},
		{"a mode that is not a string", `{"mode":1}`, multi.ID, http.StatusBadRequest, ""},
		{"an unreadable body", `{"mode":`, multi.ID, http.StatusBadRequest, ""},
		{"an empty body", ``, multi.ID, http.StatusBadRequest, ""},
		{"a single-node deployment", `{"mode":"auto"}`, single.ID, http.StatusBadRequest, ErrorPlacementModeShape},
		{"a single-node deployment to manual", `{"mode":"manual"}`, single.ID, http.StatusBadRequest, ErrorPlacementModeShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.setMode(tc.id, tc.body)
			code, params := errorBody(t, rec.Body.String())
			if rec.Code != tc.status || (tc.code != "" && code != tc.code) {
				t.Fatalf("PUT %q = %d %s, want %d %s", tc.body, rec.Code, rec.Body, tc.status, tc.code)
			}
			if tc.code == ErrorFieldInvalid && params["field"] != "mode" {
				t.Fatalf("params = %v, want field mode", params)
			}
			if !r.storedDeployment(t, multi.ID).UnitsAuto || r.storedDeployment(t, single.ID).UnitsAuto {
				t.Fatal("a refused request changed a mode")
			}
		})
	}
	// A single-node deployment has no placement_mode on the wire.
	got := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", single.ID), "")
	if got.Code != http.StatusOK || strings.Contains(got.Body.String(), `"placement_mode"`) {
		t.Fatalf("single-node GET = %d %s", got.Code, got.Body)
	}
}

// Handing a PD deployment to the platform forgets the cards its units were
// pinned to - the platform picks cards when it places them - and keeps the
// nodes. Handing it back keeps whatever the units say then.
func TestPlacementModeSwitchesAPDDeployment(t *testing.T) {
	r := newAutoRig(t, 2)
	manual := strings.Replace(r.pdAutoBody("pd-pinned", `{"node_uuid":"au-1","gpus":[1]}`, `{"node_uuid":"au-2","gpus":[0]}`, ""), `"placement_mode":"auto",`, ``, 1)
	pd := r.mustCreate(t, manual)
	if pd.PlacementMode != "manual" || !strings.Contains(r.storedDeployment(t, pd.ID).Placement, `"gpus":[1]`) {
		t.Fatalf("created = mode %q placement %s", pd.PlacementMode, r.storedDeployment(t, pd.ID).Placement)
	}

	rec := r.setMode(pd.ID, `{"mode":"auto"}`)
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.PlacementMode != "auto" || out.PD == nil {
		t.Fatalf("to auto = %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(pdNodes(t, out, "prefill"), []string{"au-1"}) || !slices.Equal(pdNodes(t, out, "decode"), []string{"au-2"}) {
		t.Fatalf("units moved: prefill %v decode %v", pdNodes(t, out, "prefill"), pdNodes(t, out, "decode"))
	}
	stored := r.storedDeployment(t, pd.ID)
	if !stored.UnitsAuto || strings.Contains(stored.Placement, `"gpus"`) || !strings.Contains(stored.Placement, `"node_uuid":"au-1"`) {
		t.Fatalf("stored auto %v placement %s, want the nodes kept and the pinned cards dropped", stored.UnitsAuto, stored.Placement)
	}

	rec = r.setMode(pd.ID, `{"mode":"manual"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	stored = r.storedDeployment(t, pd.ID)
	if rec.Code != http.StatusOK || out.PlacementMode != "manual" || stored.UnitsAuto || strings.Contains(stored.Placement, `"gpus"`) || !slices.Equal(pdNodes(t, out, "decode"), []string{"au-2"}) {
		t.Fatalf("back to manual = %d %s, stored placement %s", rec.Code, rec.Body, stored.Placement)
	}
	// Back to manual, a PD update has to name its nodes again.
	if status, body, _ := r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"au-1"},{}]}}}`); status != http.StatusBadRequest {
		t.Fatalf("manual PD update with a nodeless unit = %d %s", status, body)
	}
}
