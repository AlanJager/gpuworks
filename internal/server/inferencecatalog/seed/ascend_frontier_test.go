package seed

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

func TestFrontierAscendProfilesKeepUpstreamValidatedRecipes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	type expected struct {
		model, spec, flag, value string
	}
	cases := []expected{
		{"minimaxai/minimax-m3", "int8-vllm-ascend-a3-tp4-dp4", "--max-model-len", "133000"},
		{"minimaxai/minimax-m3", "int8-vllm-ascend-a3-tp4-dp4", "--compilation-config", `{"cudagraph_mode":"FULL_DECODE_ONLY"}`},
		{"qwen/qwen3.8-flash-next", "int8-vllm-ascend-a3-tp8", "--max-model-len", "135168"},
		{"qwen/qwen3.8-flash-next", "int8-vllm-ascend-a3-tp8", "--quantization", "ascend"},
		{"qwen/qwen3.8-flash-next", "int8-vllm-ascend-a3-tp8", "--compilation-config", `{"cudagraph_capture_sizes":[4,8,12,16,20,24,28,32],"cudagraph_mode":"FULL_DECODE_ONLY"}`},
		{"qwen/qwen3.8-flash-next", "int8-vllm-ascend-a3-tp8", "--speculative-config", `{"method":"qwen3_5_mtp","num_speculative_tokens":3,"enforce_eager":true}`},
		{"deepseek-ai/deepseek-v4.1-flash", "int8-vllm-ascend-a3-tp4-dp4", "--data-parallel-size", "4"},
		{"deepseek-ai/deepseek-v4.1-flash", "int8-vllm-ascend-a3-tp4-dp4", "--engram-config", `{"cpu_offload":true,"dp_shared_memory":true}`},
		{"deepseek-ai/deepseek-v4.1-flash-1017", "int8-vllm-ascend-a3-tp4-dp4", "--data-parallel-size", "4"},
		{"deepseek-ai/deepseek-v4.1-flash-1017", "int8-vllm-ascend-a3-tp4-dp4", "--engram-config", `{"cpu_offload":true,"dp_shared_memory":true}`},
		{"deepseek-ai/deepseek-v4-flash-0731", "int8-vllm-ascend-a3-dspark-3977f56e", "--data-parallel-size", "4"},
		{"deepseek-ai/deepseek-v4-flash-0731", "int8-vllm-ascend-a3-dspark-3977f56e", "--speculative-config", `{"method":"dspark","num_speculative_tokens":7,"enforce_eager":true}`},
		{"qwen/qwen3.5-397b-a17b", "int8-vllm-ascend-a3-w8a8-mtp-99f2a045", "--tensor-parallel-size", "16"},
		{"qwen/qwen3.5-397b-a17b", "int8-vllm-ascend-a3-w8a8-mtp-99f2a045", "--max-model-len", "133000"},
		{"qwen/qwen3.5-397b-a17b", "int8-vllm-ascend-a3-w8a8-mtp-99f2a045", "--speculative-config", `{"method":"qwen3_5_mtp","num_speculative_tokens":3,"enforce_eager":true}`},
		{"moonshotai/kimi-k2.6", "w4a8-vllm-ascend-a3-2de1ec2b", "--tensor-parallel-size", "4"},
		{"moonshotai/kimi-k2.6", "w4a8-vllm-ascend-a3-2de1ec2b", "--data-parallel-size", "4"},
	}
	seen := make([]bool, len(cases))
	minimaxSeen := false
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for index, want := range cases {
				if model.ModelID == want.model && spec.Name == want.spec && containsArgumentPair(spec.Args, want.flag, want.value) {
					seen[index] = true
				}
			}
			if model.ModelID == "minimaxai/minimax-m3" && spec.Name == "int8-vllm-ascend-a3-tp4-dp4" {
				minimaxSeen = true
			}
		}
	}
	for index, want := range cases {
		if !seen[index] {
			t.Errorf("%s/%s lacks upstream-validated %s %s", want.model, want.spec, want.flag, want.value)
		}
	}
	if !minimaxSeen {
		t.Fatal("MiniMax-M3 910C profile is missing")
	}
}

func TestQwen38BF16A3KeepsMeasuredTwoDieRecipe(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const (
		modelID = "qwen/qwen3.8-27b"
		specID  = "bf16-vllm-ascend-a3-1d4bf0f2"
	)
	var got *inferencecatalog.InferenceSpec
	for _, model := range catalog.Models {
		if model.ModelID != modelID {
			continue
		}
		for index := range model.Specs {
			if model.Specs[index].Name == specID {
				got = &model.Specs[index]
				break
			}
		}
	}
	if got == nil || got.Placement == nil {
		t.Fatalf("missing %s/%s", modelID, specID)
	}
	if got.Image != "gpuworks/gpuworks-runner:cann9.1-a3-vllm0.26.0rc1-r2" {
		t.Errorf("image = %q", got.Image)
	}
	if got.WeightBytes != 55586114768 {
		t.Errorf("weight_bytes = %d", got.WeightBytes)
	}
	if len(got.Weights.Sources) != 2 ||
		got.Weights.Sources[0].Revision != "1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0" ||
		got.Weights.Sources[1].Revision != "e823e888ae179eb3be02c1a48899c4f828371376" {
		t.Errorf("weight sources = %+v", got.Weights.Sources)
	}
	for _, pair := range [][2]string{
		{"--dtype", "bfloat16"},
		{"--max-model-len", "131072"},
		{"--mm-encoder-tp-mode", "data"},
		{"--compilation-config", `{"cudagraph_mode":"FULL_DECODE_ONLY"}`},
	} {
		if !containsArgumentPair(got.Args, pair[0], pair[1]) {
			t.Errorf("measured recipe lacks %s %s", pair[0], pair[1])
		}
	}
	names := []string{"910C", "910C"}
	memories := []float64{64, 64}
	accepted, err := placement.Resolve(context.Background(), got.Placement.Resolver, placement.GPU{
		Count: 2, Vendor: "ascend", Names: names, MemoriesGiB: memories, MinMemoryGiB: 64,
	})
	if err != nil || accepted.Reason != "" {
		t.Fatalf("two-die placement rejected: %+v %v", accepted, err)
	}
	rejected, err := placement.Resolve(context.Background(), got.Placement.Resolver, placement.GPU{
		Count: 1, Vendor: "ascend", Names: names[:1], MemoriesGiB: memories[:1], MinMemoryGiB: 64,
	})
	if err != nil || rejected.Reason == "" {
		t.Fatalf("one-die placement was not rejected: %+v %v", rejected, err)
	}
}

func TestDeepSeekV41A3ProfilesDeclareEngramSharedMemory(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const wantShmBytes = int64(512) << 30
	for _, modelID := range []string{"deepseek-ai/deepseek-v4.1-flash", "deepseek-ai/deepseek-v4.1-flash-1017"} {
		found := false
		for _, model := range catalog.Models {
			if model.ModelID != modelID {
				continue
			}
			for _, spec := range model.Specs {
				if spec.Name != "int8-vllm-ascend-a3-tp4-dp4" {
					continue
				}
				found = true
				if spec.ShmBytes != wantShmBytes || !spec.MemlockUnlimited {
					t.Errorf("%s shared-memory contract = (%d, %v), want (%d, true)", modelID, spec.ShmBytes, spec.MemlockUnlimited, wantShmBytes)
				}
			}
		}
		if !found {
			t.Errorf("%s has no int8-vllm-ascend-a3-tp4-dp4 profile", modelID)
		}
	}
}

func TestQwen3NextA3UsesMeasuredEagerPath(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "qwen/qwen3-next-80b-a3b-instruct" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "int8-sglang-ascend-a3-7d129784" {
				continue
			}
			found = true
			if !slices.Contains(spec.Args, "--disable-cuda-graph") {
				t.Errorf("A3 profile must disable graph capture: %q", spec.Args)
			}
		}
	}
	if !found {
		t.Fatal("Qwen3-Next-80B A3 profile is missing")
	}
}

func TestLargeQuantized910CProfilesFitOneSixteenDieNode(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		model, spec, revision string
		weightBytes           int64
	}{
		"deepseek-v4-flash-0731": {
			model:       "deepseek-ai/deepseek-v4-flash-0731",
			spec:        "int8-vllm-ascend-a3-dspark-3977f56e",
			revision:    "3977f56e21184c1fd542b34cb4f07e4ca45be498",
			weightBytes: 314724355617,
		},
		"qwen3.5-397b-a17b": {
			model:       "qwen/qwen3.5-397b-a17b",
			spec:        "int8-vllm-ascend-a3-w8a8-mtp-99f2a045",
			revision:    "99f2a04549fc0de1f456a4092efaf70eaff803b3",
			weightBytes: 447236376010,
		},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			var got *inferencecatalog.InferenceSpec
			for _, model := range catalog.Models {
				if model.ModelID != want.model {
					continue
				}
				for index := range model.Specs {
					if model.Specs[index].Name == want.spec {
						got = &model.Specs[index]
						break
					}
				}
			}
			if got == nil || got.Placement == nil {
				t.Fatalf("missing single-node spec %s/%s", want.model, want.spec)
			}
			if got.WeightBytes != want.weightBytes {
				t.Fatalf("weight_bytes = %d, want %d", got.WeightBytes, want.weightBytes)
			}
			if len(got.Weights.Sources) != 1 || got.Weights.Sources[0].Revision != want.revision {
				t.Fatalf("weight source = %+v, want immutable revision %s", got.Weights.Sources, want.revision)
			}
			if name == "deepseek-v4-flash-0731" {
				if _, found := got.Env["VLLM_USE_V2_MODEL_RUNNER"]; found {
					t.Error("DSpark single-node DSA-CP recipe cannot enable the V2 model runner")
				}
				if !containsArgumentPair(got.Args, "--additional-config", `{"ascend_compilation_config":{"enable_npugraph_ex":true,"enable_static_kernel":false},"enable_cpu_binding":true,"enable_dsa_cp":true,"multistream_overlap_shared_expert":true}`) {
					t.Error("DSpark profile lost its upstream single-node DSA-CP configuration")
				}
			}
			if name == "qwen3.5-397b-a17b" {
				if !slices.Contains(got.Args, "--enforce-eager") {
					t.Error("Qwen397 rc2 profile must disable the KDA graph path")
				}
				if slices.Contains(got.Args, "--compilation-config") {
					t.Error("Qwen397 rc2 profile cannot request FULL_DECODE_ONLY while causal_conv1d_update_npu is missing")
				}
			}
			names := make([]string, 16)
			memories := make([]float64, 16)
			for index := range names {
				names[index], memories[index] = "910C", 64
			}
			accepted, err := placement.Resolve(context.Background(), got.Placement.Resolver, placement.GPU{
				Count: 16, Vendor: "ascend", Names: names, MemoriesGiB: memories, MinMemoryGiB: 64,
			})
			if err != nil || accepted.Reason != "" {
				t.Fatalf("16-die placement rejected: %+v %v", accepted, err)
			}
			rejected, err := placement.Resolve(context.Background(), got.Placement.Resolver, placement.GPU{
				Count: 8, Vendor: "ascend", Names: names[:8], MemoriesGiB: memories[:8], MinMemoryGiB: 64,
			})
			if err != nil || rejected.Reason == "" {
				t.Fatalf("8-die placement was not rejected: %+v %v", rejected, err)
			}
		})
	}
}

func TestGLM53NonFlashHasNoAscendSpecWithoutRuntimeEvidence(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "zai-org/glm-5.3" {
			continue
		}
		found = true
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor == "ascend" {
				t.Errorf("GLM-5.3 non-Flash publishes Ascend profile %s without a real-hardware result", spec.Name)
			}
		}
	}
	if !found {
		t.Fatal("zai-org/glm-5.3 is missing")
	}
}

func TestGemmaAscendProfilesUseTheMeasuredPaths(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	foundA3 := false
	foundVLLM := false
	for _, model := range catalog.Models {
		if model.ModelID != "google/gemma-3-4b-it" && model.ModelID != "google/gemma-3-27b-it" {
			continue
		}
		if !slices.Equal(model.InputModalities, []string{"text", "image"}) {
			t.Errorf("%s input modalities = %v, want text and image", model.ModelID, model.InputModalities)
		}
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "ascend" || !strings.Contains(spec.Name, "-a3-") {
				continue
			}
			predicate, err := regexp.Compile(spec.GPUs.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !predicate.MatchString("910C") {
				t.Errorf("%s A3 profile no longer covers 910C: %q", model.ModelID, spec.GPUs.Name)
			}
			if spec.Engine == "sglang" && !slices.Contains(spec.Args, "--disable-cuda-graph") {
				t.Errorf("%s A3 profile lost the eager-mode workaround", model.ModelID)
			}
			if model.ModelID == "google/gemma-3-4b-it" && spec.Engine == "vllm" {
				foundVLLM = true
				if spec.Image != "gpuworks/gpuworks-runner:cann9.1-a3-vllm0.26.0rc1-r2" ||
					!containsArgumentPair(spec.Args, "--max-model-len", "32768") {
					t.Errorf("Gemma 4B vLLM profile = %+v", spec)
				}
			}
			foundA3 = true
		}
	}
	if !foundA3 {
		t.Fatal("Gemma A3 profiles are missing")
	}
	if !foundVLLM {
		t.Fatal("Gemma 4B measured vLLM A3 profile is missing")
	}
}

func TestA3EagerProfilesKeepMeasuredGraphWorkarounds(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"openbmb/minicpm3-4b":              "bf16-sglang-ascend-a3-25156446",
		"qwen/qwen3-30b-a3b-instruct-2507": "bf16-sglang-ascend-a3-04996b03",
		"qwen/qwen3-next-80b-a3b-instruct": "int8-sglang-ascend-a3-7d129784",
		"qwen/qwen3-vl-30b-a3b-instruct":   "bf16-sglang-ascend-a3-8659b2c8",
		"qwen/qwen3.6-35b-a3b":             "bf16-sglang-ascend-a3-67436760",
		"qwen/qwen3-omni-30b-a3b-instruct": "bf16-sglang-ascend-a3-68c174cb",
	}
	seen := make(map[string]bool, len(want))
	for _, model := range catalog.Models {
		specName, ok := want[model.ModelID]
		if !ok {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != specName {
				continue
			}
			seen[model.ModelID] = true
			if !slices.Contains(spec.Args, "--disable-cuda-graph") {
				t.Errorf("%s/%s lost the measured eager-mode workaround", model.ModelID, spec.Name)
			}
		}
	}
	for model := range want {
		if !seen[model] {
			t.Errorf("%s eager A3 profile is missing", model)
		}
	}
}

func TestQwen36W8A8VLLMUsesMeasuredEagerRecipe(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const (
		modelID = "qwen/qwen3.6-35b-a3b"
		specID  = "int8-vllm-ascend-a3-1a118d71"
	)
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != modelID {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != specID {
				continue
			}
			found = true
			if spec.Image != "gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1" {
				t.Errorf("image = %q", spec.Image)
			}
			if !slices.Contains(spec.Args, "--enforce-eager") {
				t.Error("measured --enforce-eager workaround is missing")
			}
		}
	}
	if !found {
		t.Fatalf("missing %s/%s", modelID, specID)
	}
}

func TestA3OmniSpeechProfilesUseTheValidatedRunner(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"qwen/qwen3-tts-12hz-0.6b-customvoice": "bf16-vllm-ascend-a3-tts06",
		"qwen/qwen3-tts-12hz-1.7b-customvoice": "bf16-vllm-ascend-a3-760501dc",
		"inclusionai/ming-omni-tts-0.5b":       "bf16-vllm-ascend-a3-6b2e03a6",
	}
	seen := make(map[string]bool, len(want))
	for _, model := range catalog.Models {
		specName, ok := want[model.ModelID]
		if !ok {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != specName {
				continue
			}
			seen[model.ModelID] = true
			if spec.Image != "gpuworks/gpuworks-runner:cann9.1-a3-vllm-omni0.30.0-r3" {
				t.Errorf("%s/%s image = %q", model.ModelID, spec.Name, spec.Image)
			}
			if !containsArgumentPair(spec.Args, "--deploy-config", "/vllm-workspace/vllm-omni/vllm_omni/deploy/"+map[string]string{
				"qwen/qwen3-tts-12hz-0.6b-customvoice": "qwen3_tts.yaml",
				"qwen/qwen3-tts-12hz-1.7b-customvoice": "qwen3_tts.yaml",
				"inclusionai/ming-omni-tts-0.5b":       "ming_tts.yaml",
			}[model.ModelID]) {
				t.Errorf("%s/%s lost its Omni deploy config", model.ModelID, spec.Name)
			}
		}
	}
	for model := range want {
		if !seen[model] {
			t.Errorf("%s A3 Omni speech profile is missing", model)
		}
	}
}

func TestSmallTextModelsHaveMeasuredA3Profiles(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"qwen/qwen2.5-coder-0.5b-instruct":    "bf16-sglang-ascend-a3-qwen25-coder-05b",
		"qwen/qwen2.5-coder-1.5b-instruct":    "bf16-sglang-ascend-a3-qwen25-coder-15b",
		"huggingfacetb/smollm2-360m-instruct": "bf16-sglang-ascend-a3-smollm2-360m",
	}
	seen := make(map[string]bool, len(want))
	for _, model := range catalog.Models {
		specName, ok := want[model.ModelID]
		if !ok {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name == specName {
				seen[model.ModelID] = true
				if spec.Image != "gpuworks/gpuworks-runner:cann9.0-a3-sglang0.5.18-r2" {
					t.Errorf("%s A3 image = %q", model.ModelID, spec.Image)
				}
			}
		}
	}
	for model := range want {
		if !seen[model] {
			t.Errorf("%s A3 profile is missing", model)
		}
	}
}

func TestGPTOSSA3ProfileUsesMeasuredOfflineRecipe(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const (
		modelID = "openai/gpt-oss-20b"
		specID  = "bf16-vllm-ascend-a3-gptoss20"
	)
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != modelID {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != specID {
				continue
			}
			found = true
			if spec.Image != "gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1" {
				t.Errorf("image = %q", spec.Image)
			}
			if !slices.Contains(spec.Args, "--enforce-eager") {
				t.Error("measured eager-mode workaround is missing")
			}
			if spec.Env["TIKTOKEN_ENCODINGS_BASE"] != "/root/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/tiktoken-rs-0.9.1/assets" {
				t.Errorf("offline tiktoken vocabulary path = %q", spec.Env["TIKTOKEN_ENCODINGS_BASE"])
			}
		}
	}
	if !found {
		t.Fatalf("missing %s/%s", modelID, specID)
	}
}

func TestQwQ32BW8A8UsesMeasuredVLLMRecipe(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const (
		modelID = "qwen/qwq-32b"
		specID  = "int8-vllm-ascend-a3-qwq32b"
	)
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != modelID {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != specID {
				continue
			}
			found = true
			if spec.Image != "gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1" {
				t.Errorf("image = %q", spec.Image)
			}
			for _, flag := range []string{"--enforce-eager", "--quantization"} {
				if !slices.Contains(spec.Args, flag) {
					t.Errorf("measured %s workaround is missing", flag)
				}
			}
		}
	}
	if !found {
		t.Fatalf("missing %s/%s", modelID, specID)
	}
}
