package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fitParityFleet is the field fleet: one node of eight 140 GiB cards, one of
// eight 24 GiB cards and one of four 22 GiB cards, all Nvidia and online.
func fitParityFleet(t *testing.T, name string) (*apiHandlers, *store.Store) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	register := func(uuid, card, archID, cc string, cards int, gib int64) {
		t.Helper()
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		gpus := make([]proto.GPUInfo, 0, cards)
		for index := 0; index < cards; index++ {
			gpus = append(gpus, proto.GPUInfo{Index: index, Name: card, Vendor: "nvidia", ArchID: archID, ComputeCapability: cc, MemoryTotalBytes: gib << 30, Healthy: true})
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: gpus,
		}); err != nil {
			t.Fatal(err)
		}
	}
	register("h20-8x140", "NVIDIA H20-3e", "sm_90", "9.0", 8, 140)
	register("rtx3090-8x24", "NVIDIA GeForce RTX 3090", "sm_86", "8.6", 8, 24)
	register("rtx2080ti-4x22", "NVIDIA GeForce RTX 2080 Ti", "sm_75", "7.5", 4, 22)
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }}}, st
}

// fitParityTemplate registers an upstream template whose only spec carries
// weightGiB of weights and the tensor-parallel degree the catalog states.
func fitParityTemplate(t *testing.T, st *store.Store, slug string, weightGiB int64, args string) (modelID, templateID uint) {
	t.Helper()
	repo := "acme/" + slug
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: repo, Revision: "main", Name: slug, WeightBytes: weightGiB << 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: repo, DisplayTitle: slug, Source: "upstream", Architecture: "qwen3", ContextLength: 32768,
		Specs: []store.InferenceSpecRow{{
			Name: "upstream", Scenario: "balanced", Engine: "vllm", Image: "example/vllm:test", Quantization: "bf16",
			WeightBytes: weightGiB << 30, Args: args,
			WeightSources: fmt.Sprintf(`[{"provider":"huggingface","repo_id":%q}]`, repo),
			GPUs:          `{"vendor":"nvidia","cc":">=7.5"}`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(repo)
	if err != nil {
		t.Fatal(err)
	}
	return model.ID, template.ID
}

// fitParityAnswers posts one literal body to the evaluation and to the create.
func fitParityAnswers(t *testing.T, h *apiHandlers, body string) (evaluate map[string]json.RawMessage, createStatus int, createBody string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &evaluate); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	return evaluate, rec.Code, rec.Body.String()
}

// fitParityEligible reads which nodes the preview offers; a node it turns
// down carries a reason.
func fitParityEligible(t *testing.T, evaluate map[string]json.RawMessage) map[string]bool {
	t.Helper()
	var nodes []struct {
		UUID     string `json:"uuid"`
		Eligible bool   `json:"eligible"`
		Reasons  []struct {
			Code string `json:"code"`
		} `json:"reasons"`
	}
	if err := json.Unmarshal(evaluate["eligible_nodes"], &nodes); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, node := range nodes {
		out[node.UUID] = node.Eligible
		if !node.Eligible && (len(node.Reasons) == 0 || node.Reasons[0].Code == "") {
			t.Fatalf("node %s is turned down without a reason", node.UUID)
		}
	}
	return out
}

// GWKS-359: large upstream templates on a fleet of 8x140 GiB, 8x24 GiB and
// 4x22 GiB Nvidia nodes, deployed automatically. The evaluation answered
// ok=true and offered every node, and the create naming an offered node was
// refused DEPLOYMENT_NEVER_FITS. One literal body goes to both endpoints
// here: they agree on whether it can be created and on the refusal, and the
// preview offers no node that a create naming it refuses for never fitting.
func TestEvaluateAndCreateAgreeOnWhetherADeploymentFits(t *testing.T) {
	const body = `{"name":%q,"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"upstream","engine":"vllm","placement_mode":%q,"target_vendor":"nvidia","replicas":1,"nnodes":1,"quick_params":{}%s}`
	for _, tc := range []struct {
		name      string
		weightGiB int64
		specArgs  string
		extra     string // further body fields
		busy      bool   // every card that could hold it is held by a running instance
		never     bool   // no node could ever run it
		eligible  map[string]bool
	}{
		// 202 GiB on two 126 GiB-usable cards: only the big node could run it.
		{name: "fits-on-two-cards", weightGiB: 202, specArgs: `["--tensor-parallel-size","2"]`,
			eligible: map[string]bool{"h20-8x140": true, "rtx3090-8x24": false, "rtx2080ti-4x22": false}},
		// qwen3-235b: 438 GiB stated as four cards.
		{name: "fits-on-four-cards", weightGiB: 438, specArgs: `["--tensor-parallel-size","4"]`,
			eligible: map[string]bool{"h20-8x140": true, "rtx3090-8x24": false, "rtx2080ti-4x22": false}},
		// glm-5: 1404 GiB planned as one card, which no card holds.
		{name: "never-by-weight", weightGiB: 1404, never: true,
			eligible: map[string]bool{"h20-8x140": false, "rtx3090-8x24": false, "rtx2080ti-4x22": false}},
		// 202 GiB stated as one card: no single card holds it.
		{name: "never-by-card-size", weightGiB: 202, extra: `,"placement":{"tp":1}`, never: true,
			eligible: map[string]bool{"h20-8x140": false, "rtx3090-8x24": false, "rtx2080ti-4x22": false}},
		// The pool could run it, but not now: the draft stays valid, the node
		// that only has to wait stays on offer, and the create is retryable.
		// (Eight 24 GiB cards also hold 100 GiB; four 22 GiB cards do not.)
		{name: "fits-but-taken", weightGiB: 100, busy: true,
			eligible: map[string]bool{"h20-8x140": true, "rtx3090-8x24": true, "rtx2080ti-4x22": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st := fitParityFleet(t, "fit_parity_"+tc.name)
			modelID, templateID := fitParityTemplate(t, st, tc.name, tc.weightGiB, tc.specArgs)
			if tc.busy {
				for _, uuid := range []string{"h20-8x140", "rtx3090-8x24"} {
					node, err := st.NodeByUUID(uuid)
					if err != nil {
						t.Fatal(err)
					}
					occupant, err := st.CreateDeployment(&store.Deployment{Name: "occupant-" + uuid, ModelID: modelID, NodeID: node.ID, Engine: "vllm"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := st.CreateInstance(&store.Instance{UUID: "occupant-" + uuid, DeploymentID: occupant.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0,1,2,3,4,5,6,7]"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			create := func(name, mode, extra string) (int, string) {
				rec := httptest.NewRecorder()
				h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(body, name, modelID, templateID, mode, tc.extra+extra+`,"served_name":"`+name+`"`))))
				return rec.Code, rec.Body.String()
			}
			evaluate, status, created := fitParityAnswers(t, h, fmt.Sprintf(body, tc.name, modelID, templateID, "auto", tc.extra))
			if got := fitParityEligible(t, evaluate); !reflect.DeepEqual(got, tc.eligible) {
				t.Fatalf("eligible nodes = %v, want %v", got, tc.eligible)
			}
			_, selected := evaluate["selected_node"]
			switch {
			case tc.never:
				var refusal struct {
					Code   string          `json:"code"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal([]byte(created), &refusal); err != nil {
					t.Fatal(err)
				}
				wantJSON(t, string(evaluate["ok"]), `false`)
				wantJSON(t, string(evaluate["errors"]), fmt.Sprintf(`[{"field":"","code":%q,"params":%s}]`, ErrorDeploymentNeverFits, refusal.Params))
				if selected || status != http.StatusBadRequest || refusal.Code != ErrorDeploymentNeverFits {
					t.Fatalf("selected=%v create = %d %s", selected, status, created)
				}
			case tc.busy:
				wantJSON(t, string(evaluate["ok"]), `true`)
				if _, hasErrors := evaluate["errors"]; hasErrors || selected {
					t.Fatalf("a busy pool: selected=%v errors=%s", selected, evaluate["errors"])
				}
				if status != http.StatusConflict || !strings.Contains(created, `"code":"`+ErrorAutomaticPlacementUnavailable+`"`) {
					t.Fatalf("create on a busy pool = %d %s", status, created)
				}
			default:
				wantJSON(t, string(evaluate["ok"]), `true`)
				var node struct {
					UUID string `json:"uuid"`
				}
				if err := json.Unmarshal(evaluate["selected_node"], &node); err != nil || node.UUID != "h20-8x140" {
					t.Fatalf("selected node = %s (%v)", evaluate["selected_node"], err)
				}
				if status != http.StatusCreated || !strings.Contains(created, `"node_uuid":"h20-8x140"`) {
					t.Fatalf("create = %d %s", status, created)
				}
			}
			// A node the preview does not offer is one a create naming it
			// refuses as a wrong request, whichever mode names it; one it
			// offers is accepted.
			for node, offered := range tc.eligible {
				for _, mode := range []string{"auto", "manual"} {
					if tc.busy && mode == "auto" {
						continue // an automatic create still needs room now
					}
					status, answer := create(tc.name+"-"+mode+"-"+node, mode, `,"node_uuid":"`+node+`"`)
					if tc.never && !strings.Contains(answer, `"code":"`+ErrorDeploymentNeverFits+`"`) {
						t.Fatalf("%s create naming %s = %d %s, want %s", mode, node, status, answer, ErrorDeploymentNeverFits)
					}
					if offered != (status == http.StatusCreated) || (!offered && mode == "manual" && status != http.StatusBadRequest) {
						t.Fatalf("preview offers %s = %v, but a %s create naming it answers %d %s", node, offered, mode, status, answer)
					}
				}
			}
		})
	}
}
