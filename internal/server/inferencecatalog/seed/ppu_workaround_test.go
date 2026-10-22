package seed

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// ppuWorkaroundProfile binds one PPU profile to the start arguments that its
// passing run needed. The run records live outside this repository, so the
// profile name is the link back to them: the hash suffix is the tested
// revision prefix and the rest names the engine, the vendor and the property
// that made the run pass.
//
// Every entry below was read off the run that actually passed, not off the
// model family. The research worker escalates through a fixed ladder - vendor
// vLLM, GPUStack vLLM, SGLang with default graphs, SGLang without piecewise
// graphs, SGLang without any CUDA graph - and the run id records which rung
// produced the pass. A profile may only carry that rung's flags: adding a
// workaround the passing run did not use publishes an unverified runtime, and
// dropping one publishes a rung that failed.
type ppuWorkaroundProfile struct {
	model    string
	spec     string
	flags    [][2]string
	switches []string
	env      map[string]string
}

var ppuWorkaroundProfiles = []ppuWorkaroundProfile{
	{
		// Passed on the "graph-no-piecewise" rung after the default-graph SGLang
		// attempt on the same revision failed. The all-graphs-disabled rung was
		// never reached, so --disable-cuda-graph must stay out.
		model: "qwen/qwen3-14b", spec: "fp8-sglang-ppu-no-piecewise-9a283b4a",
		switches: []string{"--disable-piecewise-cuda-graph"},
	},
	{
		model: "qwen/qwen3-4b", spec: "fp8-sglang-ppu-no-piecewise-96b30dc1",
		switches: []string{"--disable-piecewise-cuda-graph"},
	},
	{
		// AWQ on PPU runs on a float16 compute dtype; the vision tower also has
		// to be replicated per tensor-parallel rank rather than split.
		model: "qwen/qwen2.5-vl-7b-instruct", spec: "int4-vllm-ppu-vision-536a3579",
		flags: [][2]string{{"--dtype", "float16"}, {"--quantization", "awq"}, {"--mm-encoder-tp-mode", "data"}},
	},
	{
		model: "qwen/qwen2.5-vl-72b-instruct", spec: "int4-vllm-ppu-vision-c8b87d4b",
		flags: [][2]string{{"--dtype", "float16"}, {"--quantization", "awq"}, {"--mm-encoder-tp-mode", "data"}},
	},
	{
		// vLLM's official V4 recipe: an FP8 KV cache in 256-token blocks. The
		// catalog's own kv_bytes_per_token for this model is already computed at
		// one byte per element, so a launch without these flags would size the
		// cache for a runtime the model was never measured against.
		model: "deepseek-ai/deepseek-v4-flash", spec: "fp8-vllm-ppu-pp4-60d8d707",
		flags: [][2]string{{"--tokenizer-mode", "deepseek_v4"}, {"--kv-cache-dtype", "fp8"}, {"--block-size", "256"}},
	},
	{
		// The MoE backend environment is this model's launch recipe across every
		// vLLM spec in the catalog, and the PPU pass used the same one.
		model: "qwen/qwen3.5-397b-a17b", spec: "int4-vllm-ppu-tp4-df333de3",
		env: map[string]string{
			"VLLM_FLASHINFER_MOE_BACKEND": "latency",
			"VLLM_USE_DEEP_GEMM":          "0",
			"VLLM_DEEP_GEMM_WARMUP":       "skip",
		},
	},
}

func TestPPUProfilesKeepTheirEvidenceBoundArguments(t *testing.T) {
	for _, expected := range ppuWorkaroundProfiles {
		t.Run(expected.model, func(t *testing.T) {
			_, spec := ppuSpec(t, expected.model, expected.spec)
			for _, pair := range expected.flags {
				if !containsArgumentPair(spec.Args, pair[0], pair[1]) {
					t.Errorf("%s no longer passes %s %s: %v", spec.Name, pair[0], pair[1], spec.Args)
				}
			}
			for _, flag := range expected.switches {
				if !slices.Contains(spec.Args, flag) {
					t.Errorf("%s no longer passes %s: %v", spec.Name, flag, spec.Args)
				}
			}
			for key, want := range expected.env {
				if spec.Env[key] != want {
					t.Errorf("%s env %s = %q, want %q", spec.Name, key, spec.Env[key], want)
				}
			}
		})
	}
}

// TestAWQSpecsPinFloat16ComputeDtype is the general form of the AWQ entries
// above. Every AWQ checkpoint in the catalog runs on the PPU float16 kernel;
// an AWQ spec that leaves the compute dtype to the checkpoint's own default
// would load a kernel that no run has exercised.
func TestAWQSpecsPinFloat16ComputeDtype(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.Weights.Quantization != "awq" || spec.Engine != "vllm" {
				continue
			}
			checked++
			if !containsArgumentPair(spec.Args, "--dtype", "float16") {
				t.Errorf("%s/%s is an AWQ vLLM spec without --dtype float16: %v", model.ModelID, spec.Name, spec.Args)
			}
			if !containsArgumentPair(spec.Args, "--quantization", "awq") {
				t.Errorf("%s/%s is an AWQ spec without an explicit --quantization awq: %v", model.ModelID, spec.Name, spec.Args)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no AWQ vLLM spec was audited; the rule would silently stop applying")
	}
}

// TestVisionModelsDeclareImageInputsAndAChatContract covers the multimodal
// models added from PPU runs. A vision checkpoint whose model row declares text
// only is published to the deploy form and the Playground as a text model: the
// multimodal parameter group stays hidden and the image path is never offered,
// even though the passing run sent real images.
//
// This asserts the declared contract, not that an image was ever sent. The two
// Qwen2.5-VL profiles were verified with a base64 data-URL image request; the
// Qwen3-VL-32B run only exercised the text path, which is why its profile name
// carries no `vision` marker. See docs/dev/ledgers/ppu-support-gaps.md.
func TestVisionModelsDeclareImageInputsAndAChatContract(t *testing.T) {
	for _, expected := range []struct{ model, spec string }{
		{"qwen/qwen2.5-vl-7b-instruct", "int4-vllm-ppu-vision-536a3579"},
		{"qwen/qwen2.5-vl-72b-instruct", "int4-vllm-ppu-vision-c8b87d4b"},
		{"qwen/qwen3-vl-32b-instruct", "fp8-sglang-ppu-4bf2c2f3"},
	} {
		t.Run(expected.model, func(t *testing.T) {
			model, spec := ppuSpec(t, expected.model, expected.spec)
			if !slices.Contains(model.InputModalities, "image") {
				t.Errorf("%s does not declare image input: %v", model.ModelID, model.InputModalities)
			}
			if !slices.Contains(model.InputModalities, "text") {
				t.Errorf("%s does not declare text input: %v", model.ModelID, model.InputModalities)
			}
			contract, err := inferenceapi.EffectiveContract(spec.API, inferenceapi.DefaultsInput{Engine: spec.Engine})
			if err != nil {
				t.Fatal(err)
			}
			definition, ok := inferenceapi.Lookup(inferenceapi.OperationChat)
			if !ok || definition.Renderer != "chat" {
				t.Fatalf("the chat operation no longer renders image parts")
			}
			var chat bool
			for _, operation := range contract.Operations {
				if operation.Type != inferenceapi.OperationChat {
					continue
				}
				chat = true
			}
			if !chat {
				t.Errorf("%s has no openai.chat operation: %+v", spec.Name, contract.Operations)
			}
			if strings.TrimSpace(contract.HealthEndpoint) == "" {
				t.Errorf("%s has no health endpoint", spec.Name)
			}
		})
	}
}
