package seed

import (
	"context"
	"regexp"
	"slices"
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
		{"zai-org/glm-5.3-flash", "int8-vllm-ascend-a3-tp16", "--max-model-len", "1048576"},
		{"zai-org/glm-5.3-flash", "int8-vllm-ascend-a3-tp16", "--kv-cache-dtype", "int8"},
		{"deepseek-ai/deepseek-v4.1-flash", "int8-vllm-ascend-a3-tp4-dp4", "--data-parallel-size", "4"},
		{"deepseek-ai/deepseek-v4.1-flash", "int8-vllm-ascend-a3-tp4-dp4", "--engram-config", `{"cpu_offload":true,"dp_shared_memory":true}`},
		{"deepseek-ai/deepseek-v4.1-flash-1017", "int8-vllm-ascend-a3-tp4-dp4", "--data-parallel-size", "4"},
		{"deepseek-ai/deepseek-v4.1-flash-1017", "int8-vllm-ascend-a3-tp4-dp4", "--engram-config", `{"cpu_offload":true,"dp_shared_memory":true}`},
		{"deepseek-ai/deepseek-v4-flash-0731", "int8-vllm-ascend-a3-dspark-3977f56e", "--data-parallel-size", "4"},
		{"deepseek-ai/deepseek-v4-flash-0731", "int8-vllm-ascend-a3-dspark-3977f56e", "--speculative-config", `{"method":"dspark","num_speculative_tokens":7,"enforce_eager":true}`},
		{"qwen/qwen3.5-397b-a17b", "int8-vllm-ascend-a3-w8a8-mtp-99f2a045", "--tensor-parallel-size", "16"},
		{"qwen/qwen3.5-397b-a17b", "int8-vllm-ascend-a3-w8a8-mtp-99f2a045", "--max-model-len", "133000"},
		{"qwen/qwen3.5-397b-a17b", "int8-vllm-ascend-a3-w8a8-mtp-99f2a045", "--speculative-config", `{"method":"qwen3_5_mtp","num_speculative_tokens":3,"enforce_eager":true}`},
	}
	seen := make([]bool, len(cases))
	glmSeen := false
	minimaxSeen := false
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for index, want := range cases {
				if model.ModelID == want.model && spec.Name == want.spec && containsArgumentPair(spec.Args, want.flag, want.value) {
					seen[index] = true
				}
			}
			if model.ModelID == "zai-org/glm-5.3-flash" && spec.Name == "int8-vllm-ascend-a3-tp16" {
				glmSeen = true
				if slices.Contains(spec.Args, "--speculative-config") {
					t.Error("GLM-5.3-Flash 910C profile re-enabled MTP, which fails on the current A3 runtime")
				}
				if slices.Contains(spec.Args, "--compilation-config") {
					t.Error("GLM-5.3-Flash 910C profile re-enabled graph compilation, which is unstable on the current A3 runtime")
				}
				if !slices.Contains(spec.Args, "--enforce-eager") {
					t.Error("GLM-5.3-Flash 910C profile must start in eager mode")
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
	if !glmSeen {
		t.Fatal("GLM-5.3-Flash 910C profile is missing")
	}
	if !minimaxSeen {
		t.Fatal("MiniMax-M3 910C profile is missing")
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

func TestGemmaAscendProfileDoesNotClaim910C(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "google/gemma-3-4b-it" {
			continue
		}
		if !slices.Equal(model.InputModalities, []string{"text"}) {
			t.Fatalf("Gemma input modalities = %v, want text only", model.InputModalities)
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-sglang-ascend-4cdeb491" {
				continue
			}
			predicate, err := regexp.Compile(spec.GPUs.Name)
			if err != nil {
				t.Fatal(err)
			}
			if predicate.MatchString("910C") {
				t.Fatalf("Gemma Ascend profile still claims 910C: %q", spec.GPUs.Name)
			}
			if !predicate.MatchString("910B2C") {
				t.Fatalf("Gemma Ascend profile no longer covers 910B: %q", spec.GPUs.Name)
			}
			return
		}
	}
	t.Fatal("Gemma Ascend profile is missing")
}
