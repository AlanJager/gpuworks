package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// An automatic single-node create may name no node: the server picks the one
// its evaluation would, and the stored deployment keeps only what the request
// stated - no pin to the node the pick happened to land on.
func TestAutomaticCreateWithoutANodePicksTheEvaluatedNode(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st) // 20 GiB of weights: only the 80 GiB cards fit
	create := func(name, extra string) (int, dto.Deployment, string) {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","quick_params":{}%s}`, name, name, modelID, extra)
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		var got dto.Deployment
		if rec.Code == http.StatusCreated {
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, got, rec.Body.String()
	}

	status, pooled, body := create("pooled", `,"node_requirements":{"pool":["inference"]}`)
	if status != http.StatusCreated {
		t.Fatalf("automatic create without a node = %d %s", status, body)
	}
	if want := map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "pool": {"inference"}}; !reflect.DeepEqual(pooled.NodeRequirements, want) {
		t.Fatalf("requirements = %v, want %v (no pin to the picked node)", pooled.NodeRequirements, want)
	}

	// Many replicas, the shape an API client scaling across a fleet sends.
	if status, many, body := create("many", `,"replicas":64`); status != http.StatusCreated || many.Replicas != 64 {
		t.Fatalf("automatic create of 64 replicas = %d %s", status, body)
	}

	// Naming the evaluated node still works exactly as before.
	if status, _, body := create("named", `,"node_uuid":"auto-big"`); status != http.StatusCreated {
		t.Fatalf("automatic create naming its node = %d %s", status, body)
	}
}

// When no node can take the deployment, the create says so with each
// candidate's reason instead of a field error about a node it never asked for.
func TestAutomaticCreateWithoutANodeReportsWhyNoneFits(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	body := fmt.Sprintf(`{"name":"nofit","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_requirements":{"gpuworks.ai/node":["auto-small"]},"quick_params":{}}`, modelID)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Every node is a candidate: the two outside the requirement say so, and
	// the one inside cannot fit the weights.
	reasons, _ := got.Params["reasons"].([]any)
	if got.Code != ErrorAutomaticPlacementUnavailable || got.Params["candidates"] != float64(3) || len(reasons) != 3 {
		t.Fatalf("body = %s, want %s with three candidates", rec.Body.String(), ErrorAutomaticPlacementUnavailable)
	}
	counts := map[any]int{}
	for _, reason := range reasons {
		counts[reason]++
	}
	if counts["node_requirements_unmet"] != 2 || counts["no_fit"] != 1 {
		t.Fatalf("reasons = %v, want two requirement misses and one no_fit", reasons)
	}
	if deps, err := st.ListDeployments(); err != nil || len(deps) != 0 {
		t.Fatalf("a refused create stored %d deployments (%v)", len(deps), err)
	}
}
