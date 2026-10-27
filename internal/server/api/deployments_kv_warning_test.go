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

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A KV prediction is advisory at every stage, including the ever-fits create
// check and the real launch planner used by the controller. Accepting a warning
// must not silently shorten the user's context or leave a deployment unlaunchable
// because another planning entry point repeats the same prediction as a veto.
func TestKVContextWarningAllowsEvaluateCreateAndStoredLaunch(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit-context=%t", explicit), func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/kv-warning", Revision: "main", Name: "kv-warning", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":36,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128,"hidden_size":4096,"max_position_embeddings":262144}`})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: model.SourceID, Source: "upstream", KVBytesPerToken: `{"vllm":1048576}`}}); err != nil {
				t.Fatal(err)
			}
			const nodeUUID = "kv-warning-node"
			if _, err := st.RegisterNode(nodeUUID, store.HashToken("token"), nodeUUID); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateNodeStatus(nodeUUID, proto.NodeStatus{
				Sys:  proto.SysInfo{Hostname: nodeUUID, GPUVendor: "nvidia", Framework: "cuda"},
				GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotalBytes: 24 << 30, Healthy: true}},
			}); err != nil {
				t.Fatal(err)
			}
			h := &apiHandlers{d: Deps{Store: st}}
			quickParams := `{}`
			wantStoredContext := 0
			if explicit {
				quickParams = `{"max_model_len":262144}`
				wantStoredContext = 262144
			}
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":%s}`, model.ID, nodeUUID, quickParams)
			preview := httptest.NewRecorder()
			h.evaluate(preview, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if preview.Code != http.StatusOK {
				t.Fatalf("evaluate=%d %s", preview.Code, preview.Body.String())
			}
			// Literal wire keys protect the response the deployment form actually reads.
			var wire struct {
				OK   bool `json:"ok"`
				Plan struct {
					OK   bool            `json:"ok"`
					Plan json.RawMessage `json:"plan"`
				} `json:"plan"`
				Breakdown struct {
					Fits              bool `json:"fits"`
					EstimatedKVTokens int  `json:"estimated_kv_tokens"`
					MaxConcurrency    *int `json:"max_concurrency"`
				} `json:"breakdown"`
				Warnings []struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				} `json:"warnings"`
				Argv []string `json:"argv"`
			}
			if err := json.Unmarshal(preview.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if !wire.OK || !wire.Plan.OK || len(wire.Plan.Plan) == 0 || string(wire.Plan.Plan) == "null" || !wire.Breakdown.Fits {
				t.Fatalf("KV warning blocked preview or dropped the GPU plan: %s", preview.Body.String())
			}
			if wire.Breakdown.MaxConcurrency == nil || *wire.Breakdown.MaxConcurrency != 0 || wire.Breakdown.EstimatedKVTokens <= 0 || wire.Breakdown.EstimatedKVTokens >= 262144 {
				t.Fatalf("preview lost the estimated zero concurrency: %s", preview.Body.String())
			}
			warningFound := false
			for _, warning := range wire.Warnings {
				if warning.Code == "PLANNING_KV_BELOW_CONTEXT" {
					warningFound = true
					if len(warning.Params) != 2 || warning.Params["kv_tokens"] != float64(wire.Breakdown.EstimatedKVTokens) || warning.Params["max_model_len"] != float64(262144) {
						t.Fatalf("warning wire params = %+v", warning.Params)
					}
				}
			}
			if !warningFound {
				t.Fatalf("preview omitted the KV risk warning: %s", preview.Body.String())
			}
			assertContext := func(argv []string) {
				t.Helper()
				index := slices.Index(argv, "--max-model-len")
				if !explicit {
					if index >= 0 {
						t.Fatalf("model-limit estimate injected an explicit context: %v", argv)
					}
					return
				}
				if index < 0 || index+1 >= len(argv) || argv[index+1] != "262144" {
					t.Fatalf("launch changed the user's context: %v", argv)
				}
			}
			assertContext(wire.Argv)
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create refused after a warning: %d %s", created.Code, created.Body.String())
			}
			deployments, err := st.ListDeployments()
			if err != nil || len(deployments) != 1 {
				t.Fatalf("deployments=%+v err=%v", deployments, err)
			}
			var quick deploy.LaunchConfig
			if err := json.Unmarshal([]byte(deployments[0].QuickParams), &quick); err != nil || quick.MaxModelLen != wantStoredContext {
				t.Fatalf("stored context was rewritten: %s err=%v", deployments[0].QuickParams, err)
			}
			node, err := st.NodeWithGPUs(nodeUUID)
			if err != nil {
				t.Fatal(err)
			}
			request, layer, _, err := planning.DeploymentLaunch(&deployments[0], nil, node, model, &planning.ModelPaths{Primary: "/models/kv-warning"})
			if err != nil {
				t.Fatal(err)
			}
			request.APIPort = 30000
			request.Topology = planning.DistTopology{NNodes: 1}
			plan, refusal, err := planning.PlanTemplateLaunch(context.Background(), st, h.networks(), node, request, layer)
			if err != nil || refusal != nil || !plan.Output.Result.OK || plan.Output.Result.Plan == nil {
				t.Fatalf("stored launch refused the KV risk: refusal=%+v err=%v result=%+v", refusal, err, plan.Output.Result)
			}
			if !plan.Output.Breakdown.ContextExceedsKVPool() || !plan.Output.Breakdown.Fits {
				t.Fatalf("stored launch lost the advisory KV risk: %+v", plan.Output.Breakdown)
			}
			assertContext(plan.Resolved.Argv)
		})
	}
}

// PD does its own rank loop, so a fitting first decode rank must not hide the
// prefill ranks' smaller KV capacity. Every unit still has a runnable GPU plan.
func TestKVContextWarningFromPDPrefillKeepsEveryUnitPlaceable(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: "org/pd", Source: "upstream", KVBytesPerToken: `{"vllm":1048576}`}}); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(pdBody(modelID, pdOneEach, ""), `"max_model_len":4096`, `"max_model_len":100000`, 1)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PD evaluate=%d %s", rec.Code, rec.Body.String())
	}
	var response dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.PD == nil || len(response.PD.Roles) != 2 || len(response.Errors) > 0 {
		t.Fatalf("PD warning blocked the preview: %s", rec.Body.String())
	}
	if got := breakdownNumber(t, response.Breakdown, "estimated_kv_tokens"); got < 100000 {
		t.Fatalf("first decode must fit the context, got %.0f tokens", got)
	}
	var prefillTokens float64
	for _, role := range response.PD.Roles {
		for _, unit := range role.Units {
			if !unit.Fits || len(unit.GPUs) == 0 || unit.Reason != nil {
				t.Fatalf("PD warning removed a runnable unit: %+v", unit)
			}
			if value, _ := flagValue(unit.Argv, "--max-model-len"); value != "100000" {
				t.Fatalf("PD warning rewrote the context: %v", unit.Argv)
			}
			if role.Role == "prefill" {
				prefillTokens = breakdownNumber(t, unit.Breakdown, "estimated_kv_tokens")
				if prefillTokens <= 0 || prefillTokens >= 100000 || breakdownNumber(t, unit.Breakdown, "max_concurrency") != 0 {
					t.Fatalf("prefill must have a real KV shortfall: %+v", unit.Breakdown)
				}
			}
		}
	}
	if prefillTokens == 0 {
		t.Fatal("fixture has no prefill unit")
	}
	warnings := 0
	for _, warning := range response.Warnings {
		if warning.Code == "PLANNING_KV_BELOW_CONTEXT" {
			warnings++
			if warning.Params["kv_tokens"] != prefillTokens || warning.Params["max_model_len"] != float64(100000) {
				t.Fatalf("PD warning does not describe the prefill risk: %+v, prefill tokens %.0f", warning, prefillTokens)
			}
		}
	}
	if warnings == 0 {
		t.Fatalf("PD preview hid prefill KV risk: %s", rec.Body.String())
	}
}

// The first rank supplies the top-level breakdown; a smaller follower can be
// the only rank with a KV shortfall. Swapping node order independently exposes
// that follower's breakdown and checks that the first response warned about
// that capacity, rather than reusing the healthy leader's capacity.
func TestKVContextWarningFromDistributedFollowerSurvivesRankAggregation(t *testing.T) {
	_, modelID, st := newParallelEvaluateWith(t, nil)
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: "org/parallel", Source: "upstream", KVBytesPerToken: `{"vllm":1048576}`}}); err != nil {
		t.Fatal(err)
	}
	status := nodeStatusTwoGPUs("par-b", 1)
	for index := range status.GPUs {
		status.GPUs[index].MemoryTotalBytes = 24 << 30
	}
	if err := st.UpdateNodeStatus("par-b", status); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	evaluate := func(nodes string) (dto.EvaluateResponse, string) {
		t.Helper()
		body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","nnodes":2,"node_uuids":%s,"placement":{"tp":1,"pp":2},"quick_params":{"max_model_len":100000}}`, modelID, nodes)
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("distributed evaluate=%d %s", rec.Code, rec.Body.String())
		}
		var out dto.EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.OK || len(out.Errors) != 0 || len(out.RankCommands) != 2 {
			t.Fatalf("follower warning blocked distributed preview: %s", rec.Body.String())
		}
		for _, rank := range out.RankCommands {
			if value, _ := flagValue(rank.Argv, "--max-model-len"); value != "100000" {
				t.Fatalf("rank %s lost the requested context: %v", rank.NodeUUID, rank.Argv)
			}
		}
		return out, rec.Body.String()
	}
	leader, leaderRaw := evaluate(`["par-a","par-b"]`)
	follower, followerRaw := evaluate(`["par-b","par-a"]`)
	leaderTokens := breakdownNumber(t, leader.Breakdown, "estimated_kv_tokens")
	followerTokens := breakdownNumber(t, follower.Breakdown, "estimated_kv_tokens")
	if leaderTokens < 100000 || followerTokens <= 0 || followerTokens >= 100000 || breakdownNumber(t, follower.Breakdown, "max_concurrency") != 0 {
		t.Fatalf("fixture does not isolate follower-only KV risk: leader %.0f follower %.0f; %s", leaderTokens, followerTokens, followerRaw)
	}
	var plan struct {
		OK   bool            `json:"ok"`
		Plan json.RawMessage `json:"plan"`
	}
	encoded, err := json.Marshal(leader.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.OK || len(plan.Plan) == 0 || string(plan.Plan) == "null" {
		t.Fatalf("distributed preview lost its GPU plan: %s", leaderRaw)
	}
	warnings := 0
	for _, warning := range leader.Warnings {
		if warning.Code == "PLANNING_KV_BELOW_CONTEXT" {
			warnings++
			if warning.Params["kv_tokens"] != followerTokens || warning.Params["max_model_len"] != float64(100000) {
				t.Fatalf("warning substituted leader capacity for follower risk: %+v; leader %.0f follower %.0f", warning, leaderTokens, followerTokens)
			}
		}
	}
	if warnings == 0 {
		t.Fatalf("distributed preview hid follower KV risk: %s", leaderRaw)
	}
}
