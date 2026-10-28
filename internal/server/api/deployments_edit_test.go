package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func withID(req *http.Request, id uint) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(id))
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func getDraft(t *testing.T, h *apiHandlers, id uint) (dto.DeploymentDraft, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.getDeploymentDraft(rec, withID(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/draft", id), nil), id))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft = %d %s", rec.Code, rec.Body)
	}
	var draft dto.DeploymentDraft
	if err := json.Unmarshal(rec.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	return draft, rec.Body.String()
}

func putEdit(t *testing.T, h *apiHandlers, id uint, body any, dryRun bool) (int, string) {
	t.Helper()
	raw, ok := body.(string)
	if !ok {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(encoded)
	}
	path := fmt.Sprintf("/api/v1/deployments/%d", id)
	if dryRun {
		path += "?dry_run=1"
	}
	rec := httptest.NewRecorder()
	h.editDeployment(rec, withID(httptest.NewRequest(http.MethodPut, path, strings.NewReader(raw)), id))
	return rec.Code, rec.Body.String()
}

func decodePlan(t *testing.T, body string) dto.DeploymentEditPlan {
	t.Helper()
	var plan dto.DeploymentEditPlan
	if err := json.Unmarshal([]byte(body), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func decodeEditResult(t *testing.T, body string) dto.DeploymentEditResult {
	t.Helper()
	var result dto.DeploymentEditResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func deploymentEventCodes(t *testing.T, st *store.Store, id uint) []string {
	t.Helper()
	events, err := st.DeploymentEvents(id, 100)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, event := range events {
		codes = append(codes, event.Code)
	}
	return codes
}

// The draft is the create request that would make the deployment, on the
// wire in the create's own field names; sending it back unchanged changes
// nothing and records nothing.
func TestDeploymentDraftRoundTripsUnchanged(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	draft, raw := getDraft(t, h, dep.ID)
	for _, want := range []string{
		`"deployment":{`, `"placement_mode":"manual"`, `"node_uuid":"auto-big"`, `"target_vendor":"nvidia"`,
		`"replicas":2`, `"engine":"vllm"`, `"image":"example/vllm:test"`, `"health_check":`, `"tokenize_enabled":true`,
		`"revision":"`, `"locked":["model_id",`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("draft lacks %s\n%s", want, raw)
		}
	}
	if strings.Contains(raw, `"gpuworks.ai/node"`) || strings.Contains(raw, `"gpuworks.ai/vendor"`) {
		t.Fatalf("the pin and the vendor are stated as node_uuid and target_vendor, not as requirements: %s", raw)
	}
	if slices.Contains(draft.Locked, "replicas") || slices.Contains(draft.Locked, "quick_params") {
		t.Fatalf("a single-node deployment's replicas and launch are editable: %v", draft.Locked)
	}
	again, _ := getDraft(t, h, dep.ID)
	if again.Revision != draft.Revision {
		t.Fatalf("revision moved between two reads: %s %s", draft.Revision, again.Revision)
	}

	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision}, true)
	if status != http.StatusOK {
		t.Fatalf("dry run = %d %s", status, body)
	}
	if plan := decodePlan(t, body); len(plan.Changes) != 0 || plan.Restart {
		t.Fatalf("an unchanged draft planned changes: %s", body)
	}
	status, body = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	if slices.Contains(deploymentEventCodes(t, st, dep.ID), store.EventDeploymentEdited) {
		t.Fatal("an edit that changed nothing was recorded")
	}
	after, _ := getDraft(t, h, dep.ID)
	if after.Revision != draft.Revision {
		t.Fatal("an edit that changed nothing moved the revision")
	}
}

// A launch change is planned as a restart; with no replica serving, the
// edit replaces them all by default. The stored configuration keeps every
// key the edit did not touch, the deployment now deviates from its
// template, and the timeline records which fields changed.
func TestEditDeploymentLaunchChange(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	draft, _ := getDraft(t, h, dep.ID)
	before, _ := st.GetDeployment(dep.ID)
	edit := draft.Deployment
	edit.QuickParams = cloneAnyMap(edit.QuickParams)
	edit.QuickParams["max_model_len"] = 4096
	// Flags the dictionary describes move into their fields, as on create.
	edit.ExtraArgs = "--enforce-eager --max-num-seqs 64 --custom-flag x"

	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, true)
	if status != http.StatusOK {
		t.Fatalf("dry run = %d %s", status, body)
	}
	plan := decodePlan(t, body)
	fields := map[string]string{}
	for _, change := range plan.Changes {
		fields[change.Field] = change.Layer
	}
	if fields["quick_params.max_model_len"] != dto.DeploymentChangeRestart || fields["quick_params.max_num_seqs"] != dto.DeploymentChangeRestart ||
		fields["quick_params.enforce_eager"] != dto.DeploymentChangeRestart || fields["extra_args"] != dto.DeploymentChangeRestart {
		t.Fatalf("changes = %s", body)
	}
	if !plan.Restart || plan.DefaultApply != "all" || !slices.Equal(plan.ApplyOptions, []string{"all", "none"}) {
		t.Fatalf("plan for a deployment with no replica serving = %s", body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.QuickParams != before.QuickParams {
		t.Fatal("a dry run wrote the edit")
	}

	status, body = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	result := decodeEditResult(t, body)
	if result.Plan.Revision == draft.Revision {
		t.Fatal("the answer carries the revision the edit replaced")
	}
	row, _ := st.GetDeployment(dep.ID)
	quick, _ := storedObject(row.QuickParams)
	if quick["max_model_len"] != float64(4096) || quick["max_num_seqs"] != float64(64) || quick["enforce_eager"] != true {
		t.Fatalf("stored quick params = %s", row.QuickParams)
	}
	beforeQuick, _ := storedObject(before.QuickParams)
	for key, value := range beforeQuick {
		if key == "max_model_len" || key == "max_num_seqs" || key == "enforce_eager" {
			continue
		}
		if !sameJSON(quick[key], value) {
			t.Fatalf("untouched key %s moved: %v -> %v", key, value, quick[key])
		}
	}
	if row.ExtraArgs != "--custom-flag x" || !row.TemplateDeviated {
		t.Fatalf("extra_args=%q deviated=%v", row.ExtraArgs, row.TemplateDeviated)
	}
	if !slices.Contains(deploymentEventCodes(t, st, dep.ID), store.EventDeploymentEdited) {
		t.Fatal("the edit is not on the timeline")
	}
}

// Changing only what applies at once restarts nothing.
func TestEditDeploymentLiveChangeRestartsNothing(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	if _, err := st.CreateInstance(&store.Instance{UUID: "live-0", DeploymentID: dep.ID, NodeID: nodeIDOf(t, st, "auto-big"), State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	draft, _ := getDraft(t, h, dep.ID)
	edit := draft.Deployment
	edit.Replicas = 3
	edit.RoutingPolicy = "least_request"
	edit.StartTimeoutSec = 900
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision, Apply: "rolling"}, false)
	if status != http.StatusBadRequest || !strings.Contains(body, ErrorDeploymentEditApplyInvalid) {
		t.Fatalf("a restart asked of a live-only edit = %d %s", status, body)
	}
	status, body = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	result := decodeEditResult(t, body)
	if result.Plan.Restart || len(result.Plan.ApplyOptions) != 0 {
		t.Fatalf("live-only plan = %s", body)
	}
	for _, change := range result.Plan.Changes {
		if change.Layer != dto.DeploymentChangeLive {
			t.Fatalf("change %s is not live", change.Field)
		}
	}
	row, _ := st.GetDeployment(dep.ID)
	if row.Replicas != 3 || row.RoutingPolicy != "least_request" || row.StartTimeoutSec != 900 || row.RestartRequestedAt != nil || row.TemplateDeviated {
		t.Fatalf("after a live edit: replicas=%d routing=%s timeout=%d restart=%v deviated=%v", row.Replicas, row.RoutingPolicy, row.StartTimeoutSec, row.RestartRequestedAt, row.TemplateDeviated)
	}
	if inst, _ := st.InstanceByUUID("live-0"); inst.State != proto.InstRunning {
		t.Fatalf("a live edit touched an instance: %s", inst.State)
	}
}

// With two replicas and one serving, a launch change rolls by default; the
// edit and the rolling restart land together.
func TestEditDeploymentRollsWhenAReplicaServes(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	if _, err := st.CreateInstance(&store.Instance{UUID: "roll-0", DeploymentID: dep.ID, NodeID: nodeIDOf(t, st, "auto-big"), State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	draft, _ := getDraft(t, h, dep.ID)
	edit := draft.Deployment
	edit.QuickParams = cloneAnyMap(edit.QuickParams)
	edit.QuickParams["max_model_len"] = 8192
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	plan := decodeEditResult(t, body).Plan
	if plan.DefaultApply != "rolling" || !slices.Equal(plan.ApplyOptions, []string{"rolling", "all", "none"}) {
		t.Fatalf("plan = %+v", plan)
	}
	if row, _ := st.GetDeployment(dep.ID); row.RestartRequestedAt == nil {
		t.Fatal("the rolling restart was not asked for")
	}
	if inst, _ := st.InstanceByUUID("roll-0"); inst.State != proto.InstRunning {
		t.Fatalf("the serving replica went before its turn: %s", inst.State)
	}
}

func TestEditDeploymentRefusals(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	dep := editableDeployment(t, h, modelID)
	draft, _ := getDraft(t, h, dep.ID)
	before, _ := st.GetDeployment(dep.ID)
	locked := draft.Deployment
	locked.ModelID = modelID + 100
	engine := draft.Deployment
	engine.Engine = "sglang"
	served := draft.Deployment
	served.ServedName = "renamed"
	big := draft.Deployment
	big.Placement = map[string]any{"tp": 8}
	for name, c := range map[string]struct {
		body   any
		status int
		want   string
	}{
		"model":                     {dto.DeploymentEditRequest{Deployment: locked, BaseRevision: draft.Revision}, http.StatusBadRequest, `"field":"model_id"`},
		"engine":                    {dto.DeploymentEditRequest{Deployment: engine, BaseRevision: draft.Revision}, http.StatusBadRequest, `"field":"engine"`},
		"served name":               {dto.DeploymentEditRequest{Deployment: served, BaseRevision: draft.Revision}, http.StatusBadRequest, `"field":"served_name"`},
		"stale revision":            {dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: "0000"}, http.StatusConflict, ErrorDeploymentChanged},
		"no revision":               {dto.DeploymentEditRequest{Deployment: draft.Deployment}, http.StatusBadRequest, `"field":"base_revision"`},
		"unknown field":             {`{"deployment":{},"base_revision":"x","surprise":1}`, http.StatusBadRequest, `"field":"body"`},
		"bad extra args":            {dto.DeploymentEditRequest{Deployment: withExtra(draft.Deployment, `--x "unterminated`), BaseRevision: draft.Revision}, http.StatusBadRequest, ErrorExtraArgsInvalid},
		"JSON flag lost its quotes": {dto.DeploymentEditRequest{Deployment: withExtra(draft.Deployment, `--compilation-config {"a":1}`), BaseRevision: draft.Revision}, http.StatusBadRequest, ErrorExtraArgsJSONInvalid},
		"bad routing":               {dto.DeploymentEditRequest{Deployment: withRouting(draft.Deployment, "sideways"), BaseRevision: draft.Revision}, http.StatusBadRequest, ErrorRoutingPolicyInvalid},
		"never fits":                {dto.DeploymentEditRequest{Deployment: big, BaseRevision: draft.Revision}, http.StatusBadRequest, ErrorDeploymentNeverFits},
		"start when active":         {dto.DeploymentEditRequest{Deployment: withExtra(draft.Deployment, "--custom-flag x"), BaseRevision: draft.Revision, Start: true}, http.StatusBadRequest, ErrorDeploymentEditApplyInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := putEdit(t, h, dep.ID, c.body, false)
			if status != c.status || !strings.Contains(body, c.want) {
				t.Fatalf("= %d %s, want %d with %s", status, body, c.status, c.want)
			}
		})
	}
	if row, _ := st.GetDeployment(dep.ID); row.QuickParams != before.QuickParams || row.ExtraArgs != before.ExtraArgs || row.RoutingPolicy != before.RoutingPolicy {
		t.Fatal("a refused edit wrote something")
	}

	if err := st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision}, false); status != http.StatusConflict || !strings.Contains(body, ErrorDeploymentDeleting) {
		t.Fatalf("editing a deleting deployment = %d %s", status, body)
	}
	if status, _ := putEdit(t, h, 9999, dto.DeploymentEditRequest{}, false); status != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", status)
	}
}

// A stopped deployment saves the edit; asked to, it starts with it.
func TestEditStoppedDeploymentSavesOrStarts(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	if err := st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	draft, _ := getDraft(t, h, dep.ID)
	edit := withExtra(draft.Deployment, "--custom-flag x")
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, true)
	plan := decodePlan(t, body)
	if status != http.StatusOK || !plan.CanStart || plan.DefaultApply != "none" || !slices.Equal(plan.ApplyOptions, []string{"none"}) {
		t.Fatalf("stopped plan = %d %s", status, body)
	}
	status, body = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision, Start: true}, false)
	if status != http.StatusOK {
		t.Fatalf("save and start = %d %s", status, body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.State != store.DeployActive || row.ExtraArgs != "--custom-flag x" {
		t.Fatalf("after save and start: state=%s extra=%q", row.State, row.ExtraArgs)
	}
}

// Requirements and the manual pin are edited as on create: moving the pin
// to another node is a live change of where the next replicas run.
func TestEditDeploymentMovesThePin(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	draft, _ := getDraft(t, h, dep.ID)
	edit := draft.Deployment
	edit.NodeUUID = "auto-labelled"
	edit.PlacementStrategy = "binpack"
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	plan := decodeEditResult(t, body).Plan
	fields := map[string]bool{}
	for _, change := range plan.Changes {
		fields[change.Field] = true
	}
	if !fields["node_requirements"] || !fields["node_uuid"] || !fields["placement_strategy"] || plan.Restart {
		t.Fatalf("plan = %s", body)
	}
	reqs, _ := st.DeploymentRequirements(dep.ID)
	if got := reqs["gpuworks.ai/node"]; !slices.Equal(got, []string{"auto-labelled"}) {
		t.Fatalf("pin = %v", got)
	}
	if row, _ := st.GetDeployment(dep.ID); row.NodeID != nodeIDOf(t, st, "auto-labelled") || row.PlacementStrategy != store.PlacementBinpack {
		t.Fatalf("node=%d strategy=%s", row.NodeID, row.PlacementStrategy)
	}
}

// An evaluation of an edit leaves out what the deployment's own instances
// hold: a replica filling the card does not make its own edit unplaceable.
func TestEvaluateForAnEditExcludesTheDeploymentsOwnUsage(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	dep := editableDeployment(t, h, modelID)
	if _, err := st.CreateInstance(&store.Instance{UUID: "own-0", DeploymentID: dep.ID, NodeID: nodeIDOf(t, st, "auto-big"), State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: `{"engine":"vllm"}`}); err != nil {
		t.Fatal(err)
	}
	evaluate := func(extra string) string {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","placement_mode":"manual","target_vendor":"nvidia"%s}`, modelID, extra)
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	scheduleOK := func(body string) bool {
		var resp struct {
			Plan struct {
				OK bool `json:"ok"`
			} `json:"plan"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Plan.OK
	}
	if scheduleOK(evaluate("")) {
		t.Fatal("a new deployment fit a card another replica fills")
	}
	if body := evaluate(fmt.Sprintf(`,"deployment_id":%d`, dep.ID)); !scheduleOK(body) {
		t.Fatalf("the edit was refused for the memory its own replica holds: %s", body)
	}
}

func nodeIDOf(t *testing.T, st *store.Store, uuid string) uint {
	t.Helper()
	node, err := st.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return node.ID
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func withExtra(req dto.DeployRequest, extra string) dto.DeployRequest {
	req.ExtraArgs = extra
	return req
}

func withRouting(req dto.DeployRequest, policy string) dto.DeployRequest {
	req.RoutingPolicy = policy
	return req
}

// A deployment made from a template keeps what the template gave it through
// an edit, and an edit does not pick up a template that changed since: the
// edit starts from what the deployment runs, not from today's catalog.
func TestEditTemplateDeploymentKeepsItsTemplateLayer(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "edit_template_layer")
	dep := createTemplateDeployment(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "node_uuid": "edit_template_layer-node",
	})
	before, _ := st.GetDeployment(dep.ID)
	beforeQuick, _ := storedObject(before.QuickParams)
	if beforeQuick["max_model_len"] != float64(32768) {
		t.Fatalf("fixture: the template's max_model_len did not reach the deployment: %s", before.QuickParams)
	}

	// The catalog moves on after the deployment was made.
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/embed"
	fixture.InputModalities = `["text"]`
	fixture.Specs[0].Name = "embed"
	fixture.Specs[0].Args = `["--max-model-len","4096","--enforce-eager"]`
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Acme/Embed","revision":"0123456789abcdef0123456789abcdef01234567"}]`
	fixture.Specs[0].APIContract = `{"health_endpoint":"/health","operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}

	draft, _ := getDraft(t, h, dep.ID)
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision}, true)
	if status != http.StatusOK || len(decodePlan(t, body).Changes) != 0 {
		t.Fatalf("an unchanged draft of a template deployment planned = %d %s", status, body)
	}
	edit := draft.Deployment
	edit.QuickParams = cloneAnyMap(edit.QuickParams)
	edit.QuickParams["gpu_memory_utilization"] = 0.8
	status, body = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision, Apply: "none"}, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, body)
	}
	changes := decodeEditResult(t, body).Plan.Changes
	if len(changes) != 1 || changes[0].Field != "quick_params.gpu_memory_utilization" {
		t.Fatalf("changes = %+v", changes)
	}
	after, _ := st.GetDeployment(dep.ID)
	quick, _ := storedObject(after.QuickParams)
	if quick["max_model_len"] != float64(32768) || quick["enforce_eager"] != nil || quick["gpu_memory_utilization"] != 0.8 {
		t.Fatalf("stored quick params = %s", after.QuickParams)
	}
	if after.CatalogSelectionSnapshot != before.CatalogSelectionSnapshot || after.Image != before.Image {
		t.Fatal("the edit rewrote the template snapshot or the image")
	}
}

// A multi-node deployment names its nodes, replicas and slices rank by rank:
// its draft locks them, and an edit that changes them is refused.
func TestMultiNodeDraftLocksItsRanks(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	big, labelled := nodeIDOf(t, st, "auto-big"), nodeIDOf(t, st, "auto-labelled")
	dep, err := st.CreateDeployment(&store.Deployment{
		Name: "multi", ServedName: "multi", ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", State: store.DeployActive,
		NodeID: big, NodeIDs: fmt.Sprintf("[%d,%d]", big, labelled), NNodes: 2, Replicas: 1,
		Placement: `{"tp":1,"pp":2}`, QuickParams: `{"max_model_len":8192}`, MasterAddr: "10.0.0.1", MasterPort: 29500,
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, raw := getDraft(t, h, dep.ID)
	if !strings.Contains(raw, `"node_uuids":["auto-big","auto-labelled"]`) || !strings.Contains(raw, `"nnodes":2`) {
		t.Fatalf("multi-node draft = %s", raw)
	}
	for _, field := range []string{"node_uuids", "replicas", "vram_limit_gib", "master_addr"} {
		if !slices.Contains(draft.Locked, field) {
			t.Fatalf("multi-node draft does not lock %s: %v", field, draft.Locked)
		}
	}
	edit := draft.Deployment
	edit.NodeUUIDs = []string{"auto-labelled", "auto-big"}
	if status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, true); status != http.StatusBadRequest || !strings.Contains(body, `"field":"node_uuids"`) {
		t.Fatalf("reordering the ranks = %d %s", status, body)
	}
}

// Resizing a dGPU slice rolls like any other launch change; turning the
// slice off (or changing its backend) is a reshape the controller can only
// carry out by retiring the whole old generation first, so the plan offers
// replacing all at once and says why, and a rolling apply is refused.
func TestEditDGPUReshapeCannotRoll(t *testing.T) {
	h, modelID := partitionPoolFixture(t, map[string]string{"reshape-node": accelerator.BackendNVIDIALogical})
	st := h.d.Store
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"name":"reshaped","model_id":%d,"engine":"vllm","node_uuid":"reshape-node","replicas":2,"quick_params":{},"vram_limit_gib":8}`, modelID)
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var dep dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	node := nodeIDOf(t, st, "reshape-node")
	for _, uuid := range []string{"reshape-0", "reshape-1"} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, Engine: "vllm", VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node, State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	draft, _ := getDraft(t, h, dep.ID)

	resized := draft.Deployment
	resized.VRAMLimitGiB = 12
	status, out := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: resized, BaseRevision: draft.Revision}, true)
	if plan := decodePlan(t, out); status != http.StatusOK || plan.ApplyNote != "" || plan.DefaultApply != "rolling" {
		t.Fatalf("resizing the slice = %d %s", status, out)
	}

	whole := draft.Deployment
	whole.VRAMLimitGiB, whole.PartitionBackend = 0, ""
	status, out = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: whole, BaseRevision: draft.Revision}, true)
	plan := decodePlan(t, out)
	if status != http.StatusOK || plan.ApplyNote != dto.DeploymentEditNotePartition || plan.DefaultApply != "all" || !slices.Equal(plan.ApplyOptions, []string{"all", "none"}) {
		t.Fatalf("turning the slice off = %d %s", status, out)
	}
	if !strings.Contains(out, `"apply_note":"partition_change"`) {
		t.Fatalf("wire plan lacks the note: %s", out)
	}
	if status, out := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: whole, BaseRevision: draft.Revision, Apply: "rolling"}, false); status != http.StatusBadRequest || !strings.Contains(out, ErrorDeploymentEditApplyInvalid) {
		t.Fatalf("rolling a reshape = %d %s", status, out)
	}
}
