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
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ADR 0020 acceptance: what the form shows, what admission enforces, what the
// engine receives and what the deployment stores are one value.

func launchConfigNode(t *testing.T, st *store.Store, uuid string) {
	t.Helper()
	if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
}

func breakdownNumber(t *testing.T, breakdown any, key string) float64 {
	t.Helper()
	raw, err := json.Marshal(breakdown)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	value, ok := decoded[key].(float64)
	if !ok {
		t.Fatalf("breakdown[%s]=%v (%T), want a number; breakdown=%s", key, decoded[key], decoded[key], raw)
	}
	return value
}

func countFlag(argv []string, flag string) int {
	count := 0
	for _, token := range argv {
		if token == flag || strings.HasPrefix(token, flag+"=") {
			count++
		}
	}
	return count
}

func flagValue(argv []string, flag string) (string, bool) {
	for i, token := range argv {
		if token == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
		if strings.HasPrefix(token, flag+"=") {
			return strings.TrimPrefix(token, flag+"="), true
		}
	}
	return "", false
}

func TestEvaluateAndCreateMaterializeLaunchConfig(t *testing.T) {
	st, err := store.Open("file:launch_config_materialize?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "lc-node")
	handler := &apiHandlers{d: Deps{Store: st}}

	t.Run("nothing stated: no flag, no stored value, estimate resolves the engine default", func(t *testing.T) {
		body := fmt.Sprintf(`{"name":"lc","served_name":"lc","model_id":%d,"node_uuid":"lc-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"max_model_len":4096}}`, model.ID)
		evaluation := httptest.NewRecorder()
		handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if evaluation.Code != http.StatusOK {
			t.Fatalf("evaluate=%d: %s", evaluation.Code, evaluation.Body.String())
		}
		var preview dto.EvaluateResponse
		if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if !preview.OK {
			t.Fatalf("preview not OK: %s", evaluation.Body.String())
		}
		argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
		// The dictionary's default utilization is what an untouched form's
		// slider showed, so it reaches the engine; the enum defaults are
		// no-ops the adapter does not spell.
		if countFlag(argv, "--gpu-memory-utilization") != 1 {
			t.Fatalf("the dictionary's default utilization must reach the engine exactly once: %v", argv)
		}
		if countFlag(argv, "--dtype") != 0 {
			t.Fatalf("a dtype nobody stated must not reach the engine: %v", argv)
		}
		// The estimate still needs a number, and takes the engine's own default
		// from the dictionary - the same value the engine will run at.
		if got := breakdownNumber(t, preview.Breakdown, "gpu_memory_utilization"); got != 0.9 {
			t.Fatalf("breakdown util=%v, want vLLM's own default 0.9", got)
		}
		if fmt.Sprint(preview.EffectiveParams["gpu_memory_utilization"]) != "0.9" {
			t.Fatalf("effective_params must report the defaulted configuration: %v", preview.EffectiveParams)
		}
		if fmt.Sprint(preview.EffectiveParams["dtype"]) != "auto" {
			t.Fatalf("effective_params must carry the dictionary's enum defaults: %v", preview.EffectiveParams)
		}

		created := httptest.NewRecorder()
		handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		if created.Code != http.StatusCreated {
			t.Fatalf("create=%d: %s", created.Code, created.Body.String())
		}
		var deployment dto.Deployment
		if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(deployment.QuickParams["gpu_memory_utilization"]) != "0.9" {
			t.Fatalf("stored quick_params must hold the defaulted configuration: %v", deployment.QuickParams)
		}
		if fmt.Sprint(deployment.QuickParams["dtype"]) != "auto" {
			t.Fatalf("stored quick_params carries the dictionary's defaults: %v", deployment.QuickParams)
		}
		if fmt.Sprint(deployment.QuickParams["max_model_len"]) != "4096" {
			t.Fatalf("stored quick_params must hold what was stated: %v", deployment.QuickParams)
		}
		if deployment.ExtraArgs != "" {
			t.Fatalf("no residual was submitted, extra_args=%q", deployment.ExtraArgs)
		}
	})

	t.Run("utilization stated: injected flag, breakdown and stored config agree", func(t *testing.T) {
		body := fmt.Sprintf(`{"name":"lc-stated","served_name":"lc-stated","model_id":%d,"node_uuid":"lc-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"max_model_len":4096,"gpu_memory_utilization":0.9}}`, model.ID)
		evaluation := httptest.NewRecorder()
		handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if evaluation.Code != http.StatusOK {
			t.Fatalf("evaluate=%d: %s", evaluation.Code, evaluation.Body.String())
		}
		var preview dto.EvaluateResponse
		if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
		if countFlag(argv, "--gpu-memory-utilization") != 1 || !strings.Contains(strings.Join(argv, " "), "--gpu-memory-utilization 0.9") {
			t.Fatalf("argv must carry exactly one explicit utilization: %v", argv)
		}
		if got := breakdownNumber(t, preview.Breakdown, "gpu_memory_utilization"); got != 0.9 {
			t.Fatalf("breakdown util=%v, want the stated 0.9", got)
		}
		if fmt.Sprint(preview.EffectiveParams["gpu_memory_utilization"]) != "0.9" {
			t.Fatalf("effective_params must expose the stated configuration: %v", preview.EffectiveParams)
		}

		created := httptest.NewRecorder()
		handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		if created.Code != http.StatusCreated {
			t.Fatalf("create=%d: %s", created.Code, created.Body.String())
		}
		var deployment dto.Deployment
		if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(deployment.QuickParams["gpu_memory_utilization"]) != "0.9" || fmt.Sprint(deployment.QuickParams["max_model_len"]) != "4096" {
			t.Fatalf("stored quick_params must be what the request stated: %v", deployment.QuickParams)
		}
	})

	t.Run("field and extra_args disagree: field error on evaluate, 400 on create", func(t *testing.T) {
		body := fmt.Sprintf(`{"name":"lc-conflict","served_name":"lc-conflict","model_id":%d,"node_uuid":"lc-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"max_model_len":4096},"extra_args":"--max-model-len 8192"}`, model.ID)
		for name, call := range map[string]func(http.ResponseWriter, *http.Request){"evaluate": handler.evaluate, "create": handler.createDeployment} {
			rec := httptest.NewRecorder()
			call(rec, httptest.NewRequest(http.MethodPost, "/api/v1/"+name, strings.NewReader(body)))
			text := rec.Body.String()
			if name == "evaluate" {
				// The conflict belongs to the key's control: evaluation answers
				// 200/ok=false and the error names both values.
				if rec.Code != http.StatusOK {
					t.Fatalf("%s=%d, want 200: %s", name, rec.Code, text)
				}
				var evaluated struct {
					OK     bool             `json:"ok"`
					Errors []dto.FieldError `json:"errors"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
					t.Fatal(err)
				}
				if evaluated.OK || len(evaluated.Errors) != 1 || evaluated.Errors[0].Field != "max_model_len" || evaluated.Errors[0].Code != ErrorLaunchConfigConflict {
					t.Fatalf("%s field errors=%+v", name, evaluated.Errors)
				}
				params := evaluated.Errors[0].Params
				if fmt.Sprint(params["field_value"]) != "4096" || fmt.Sprint(params["extra_value"]) != "8192" {
					t.Fatalf("%s error must carry both values: %v", name, params)
				}
				continue
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s=%d, want 400: %s", name, rec.Code, text)
			}
			if !strings.Contains(text, ErrorLaunchConfigConflict) || !strings.Contains(text, "4096") || !strings.Contains(text, "8192") {
				t.Fatalf("%s error must name the code and both values: %s", name, text)
			}
		}
	})

	t.Run("dictionary flag in extra_args moves into the field and is echoed", func(t *testing.T) {
		body := fmt.Sprintf(`{"name":"lc-moved","served_name":"lc-moved","model_id":%d,"node_uuid":"lc-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{},"extra_args":"--max-model-len 8192 --foo"}`, model.ID)
		evaluation := httptest.NewRecorder()
		handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if evaluation.Code != http.StatusOK {
			t.Fatalf("evaluate=%d: %s", evaluation.Code, evaluation.Body.String())
		}
		var preview dto.EvaluateResponse
		if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if len(preview.Moved) != 1 || preview.Moved[0].Key != "max_model_len" || preview.Moved[0].Flag != "--max-model-len" || fmt.Sprint(preview.Moved[0].Value) != "8192" {
			t.Fatalf("moved=%+v, want max_model_len 8192", preview.Moved)
		}
		if fmt.Sprint(preview.EffectiveParams["max_model_len"]) != "8192" {
			t.Fatalf("effective_params must carry the moved value: %v", preview.EffectiveParams)
		}
		argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
		if countFlag(argv, "--max-model-len") != 1 || countFlag(argv, "--foo") != 1 {
			t.Fatalf("moved flag emitted once and residual kept: %v", argv)
		}
		created := httptest.NewRecorder()
		handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		if created.Code != http.StatusCreated {
			t.Fatalf("create=%d: %s", created.Code, created.Body.String())
		}
		var deployment dto.Deployment
		if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(deployment.QuickParams["max_model_len"]) != "8192" || strings.TrimSpace(deployment.ExtraArgs) != "--foo" {
			t.Fatalf("stored config=%v extra_args=%q; want max_model_len 8192 and residual --foo", deployment.QuickParams, deployment.ExtraArgs)
		}
	})
}

func TestSpeculativeRequestMaterializesToEngineArgvAndStoredLaunchConfig(t *testing.T) {
	st, err := store.Open("file:speculative_launch_config?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/speculative-target", Revision: "main", Name: "org/speculative-target", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "speculative-node")
	handler := &apiHandlers{d: Deps{Store: st}}

	for _, tc := range []struct {
		name        string
		engine      string
		quickParams string
		assertArgv  func(*testing.T, []string)
	}{
		{
			name:        "vllm composite speculative config",
			engine:      "vllm",
			quickParams: `{"speculative_algorithm":"draft_model","speculative_draft_model":"/models/draft-vllm","speculative_tokens":5}`,
			assertArgv: func(t *testing.T, argv []string) {
				t.Helper()
				if countFlag(argv, "--speculative-config") != 1 {
					t.Fatalf("argv must contain one composite speculative config: %v", argv)
				}
				raw, ok := flagValue(argv, "--speculative-config")
				if !ok {
					t.Fatalf("missing --speculative-config value: %v", argv)
				}
				var config map[string]any
				if err := json.Unmarshal([]byte(raw), &config); err != nil {
					t.Fatalf("invalid --speculative-config JSON %q: %v", raw, err)
				}
				want := map[string]any{"method": "draft_model", "model": "/models/draft-vllm", "num_speculative_tokens": float64(5)}
				if !reflect.DeepEqual(config, want) {
					t.Fatalf("speculative config=%#v, want %#v", config, want)
				}
			},
		},
		{
			name:        "sglang independent speculative flags",
			engine:      "sglang",
			quickParams: `{"speculative_algorithm":"EAGLE3","speculative_draft_model":"/models/draft-sglang","speculative_tokens":7,"speculative_num_steps":3,"speculative_eagle_top_k":4}`,
			assertArgv: func(t *testing.T, argv []string) {
				t.Helper()
				for flag, want := range map[string]string{
					"--speculative-algorithm":        "EAGLE3",
					"--speculative-draft-model-path": "/models/draft-sglang",
					"--speculative-num-draft-tokens": "7",
					"--speculative-num-steps":        "3",
					"--speculative-eagle-topk":       "4",
				} {
					if got, ok := flagValue(argv, flag); !ok || got != want || countFlag(argv, flag) != 1 {
						t.Errorf("%s=%q present=%v count=%d, want %q exactly once; argv=%v", flag, got, ok, countFlag(argv, flag), want, argv)
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"node_uuid":"speculative-node","engine":%q,"image":"runner:test","quick_params":%s}`, "speculative-"+tc.engine, "speculative-"+tc.engine, model.ID, tc.engine, tc.quickParams)
			evaluation := httptest.NewRecorder()
			handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if evaluation.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", evaluation.Code, evaluation.Body.String())
			}
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if !preview.OK {
				t.Fatalf("preview not OK: %s", evaluation.Body.String())
			}
			argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
			tc.assertArgv(t, argv)

			created := httptest.NewRecorder()
			handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", created.Code, created.Body.String())
			}
			var deployment dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.quickParams), &want); err != nil {
				t.Fatal(err)
			}
			storedSpeculative := map[string]any{}
			for key, value := range deployment.QuickParams {
				if strings.HasPrefix(key, "speculative_") {
					storedSpeculative[key] = value
				}
			}
			if !reflect.DeepEqual(storedSpeculative, want) {
				t.Fatalf("stored speculative quick_params=%#v, want request %#v; full config=%#v", storedSpeculative, want, deployment.QuickParams)
			}
		})
	}
}

func TestEvaluateTemplateArgsReachEstimator(t *testing.T) {
	st, err := store.Open("file:launch_config_template?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := store.InferenceModel{
		ModelID: "acme/long", DisplayTitle: "Long", Source: "upstream", Architecture: "llama", ContextLength: 1048576,
		KVBytesPerToken: `{"vllm":62464}`,
		Specs: []store.InferenceSpecRow{{
			Name: "fp8-hopper", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "fp8", WeightBytes: 16 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Long-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=9.0"}`,
			Args:          `["--kv-cache-dtype","fp8","--max-model-len","200000","--gpu-memory-utilization","0.95","--max-num-seqs","16","--no-enable-flashinfer-autotune"]`,
		}},
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Acme/Long-FP8", Revision: "main", Name: "Acme/Long-FP8", WeightBytes: 16 << 30, QuantLabel: "fp8",
		Meta: `{"model_type":"llama","max_position_embeddings":1048576,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "lc-template-node")
	handler := &apiHandlers{d: Deps{Store: st}}
	body := fmt.Sprintf(`{"name":"lc-template","template_id":%d,"model_id":%d,"node_uuid":"lc-template-node","quick_params":{}}`, template.ID, model.ID)

	evaluation := httptest.NewRecorder()
	handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if evaluation.Code != http.StatusOK {
		t.Fatalf("evaluate=%d: %s", evaluation.Code, evaluation.Body.String())
	}
	var preview dto.EvaluateResponse
	if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if got := breakdownNumber(t, preview.Breakdown, "max_model_len"); got != 200000 {
		t.Fatalf("breakdown max_model_len=%v, want the template's 200000 rather than config's 1048576", got)
	}
	if got := breakdownNumber(t, preview.Breakdown, "gpu_memory_utilization"); got != 0.95 {
		t.Fatalf("breakdown util=%v, want the template's 0.95", got)
	}
	argv := append(append([]string(nil), preview.Entrypoint...), preview.Argv...)
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--max-model-len 200000", "--kv-cache-dtype fp8", "--gpu-memory-utilization 0.95", "--max-num-seqs 16", "--no-enable-flashinfer-autotune"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv misses %q: %v", want, argv)
		}
	}
	for _, flag := range []string{"--max-model-len", "--kv-cache-dtype", "--gpu-memory-utilization", "--max-num-seqs"} {
		if countFlag(argv, flag) != 1 {
			t.Errorf("%s must appear exactly once: %v", flag, argv)
		}
	}
	if preview.TemplateParams["kv_cache_dtype"] != "fp8" || fmt.Sprint(preview.TemplateParams["max_num_seqs"]) != "16" {
		t.Fatalf("template_params=%v", preview.TemplateParams)
	}
	if len(preview.TemplateResidualArgs) != 1 || preview.TemplateResidualArgs[0] != "--no-enable-flashinfer-autotune" {
		t.Fatalf("template_residual_args=%v", preview.TemplateResidualArgs)
	}
	if preview.TemplateDeviated {
		t.Fatalf("an untouched template deploy is not a deviation: %s", evaluation.Body.String())
	}

	created := httptest.NewRecorder()
	handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d: %s", created.Code, created.Body.String())
	}
	var deployment dto.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.QuickParams["kv_cache_dtype"] != "fp8" || fmt.Sprint(deployment.QuickParams["max_num_seqs"]) != "16" || fmt.Sprint(deployment.QuickParams["gpu_memory_utilization"]) != "0.95" {
		t.Fatalf("stored config must hold the template's dictionary values: %v", deployment.QuickParams)
	}
	if deployment.ExtraArgs != "" || deployment.TemplateDeviated {
		t.Fatalf("template residual belongs to the snapshot, not to user extra_args: extra_args=%q deviated=%v", deployment.ExtraArgs, deployment.TemplateDeviated)
	}
	snapshotRaw, err := json.Marshal(deployment.CatalogSelectionSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		ExtraArgs []string `json:"extra_args"`
	}
	if err := json.Unmarshal(snapshotRaw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtraArgs) != 1 || snapshot.ExtraArgs[0] != "--no-enable-flashinfer-autotune" {
		t.Fatalf("snapshot extra_args=%v, want only the residual", snapshot.ExtraArgs)
	}
}
