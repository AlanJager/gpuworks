package seed

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

const m890pSpecName = "fp8-sglang-ppu-m890p-tp8-256k"
const m890pASLLMImage = "reg.docker.alibaba-inc.com/ai_container/asllm:1.10.16-890P-pytorch2.10.0-ubuntu24.04-sail2.1.0-cuda13.0-sglang0.5.13-vllm0.23.0-py312"

var m890pModelSources = map[string]string{
	"deepseek-ai/deepseek-v4-flash-0731": "deepseek-ai/DeepSeek-V4-Flash-0731",
	"deepseek-ai/deepseek-v4-pro-0813":   "deepseek-ai/DeepSeek-V4-Pro-0813",
	"deepseek-ai/deepseek-v4.1-flash":    "deepseek-ai/DeepSeek-V4.1-Flash",
	"qwen/qwen3.8-flash-next":            "Qwen/Qwen3.8-Flash-Next-FP8",
}

// These offline tests protect the published catalog and launch contract; they
// do not reproduce the operator's hardware validation or measure inference.
func TestM890PProfilesKeepDistinctWeightSourcesAndRuntime(t *testing.T) {
	for modelID, repoID := range m890pModelSources {
		t.Run(modelID, func(t *testing.T) {
			_, spec := ppuSpec(t, modelID, m890pSpecName)
			if spec.Engine != "sglang" || spec.Image != m890pASLLMImage {
				t.Fatalf("runtime = %s / %s", spec.Engine, spec.Image)
			}
			if spec.WeightBytes <= 0 || spec.Weights.Quantization != "fp8" {
				t.Errorf("missing FP8 checkpoint size: %+v", spec.Weights)
			}
			if len(spec.Weights.Sources) == 0 {
				t.Fatal("no checkpoint source")
			}
			providers := make(map[string]bool)
			for _, source := range spec.Weights.Sources {
				if source.RepoID != repoID || source.Subfolder != "" {
					t.Errorf("source = %+v; want exact checkpoint %s", source, repoID)
				}
				if source.Provider != "huggingface" && source.Provider != "modelscope" {
					t.Errorf("unexpected provider %q", source.Provider)
				}
				if providers[source.Provider] {
					t.Errorf("duplicate source provider %q", source.Provider)
				}
				providers[source.Provider] = true
			}
			if !providers["huggingface"] {
				t.Error("canonical Hugging Face checkpoint is missing")
			}
			for _, key := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
				if spec.Env[key] != "1" {
					t.Errorf("%s = %q, want 1", key, spec.Env[key])
				}
			}
		})
	}
}

func TestM890PProfilesMatchOnlyTheDeclaredCardFamily(t *testing.T) {
	for modelID := range m890pModelSources {
		t.Run(modelID, func(t *testing.T) {
			_, spec := ppuSpec(t, modelID, m890pSpecName)
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name, CC: spec.GPUs.CC}
			for _, test := range []struct {
				name, vendor string
				want         bool
			}{
				{"ZW-M890P", "ppu", true},
				{"M890P", "ppu", true},
				{"PPU ZW-M890P 144GB", "ppu", true},
				{"ZW810E", "ppu", false},
				{"PPU-M890", "ppu", false},
				{"M890P2", "ppu", false},
				{"XM890P", "ppu", false},
				{"NVIDIA H100", "nvidia", false},
				{"ZW-M890P", "nvidia", false},
			} {
				if got := chipmatch.Evaluate(chipmatch.GPU{Name: test.name, Vendor: test.vendor}, predicate); got.Matched != test.want {
					t.Errorf("%s/%s match = %v, want %v: %s", test.vendor, test.name, got.Matched, test.want, got.Reason)
				}
			}
		})
	}
}

func TestM890PProfilesResolveEightCardsAndLaunch256KOnce(t *testing.T) {
	for modelID := range m890pModelSources {
		t.Run(modelID, func(t *testing.T) {
			_, spec := ppuSpec(t, modelID, m890pSpecName)
			if spec.Placement == nil {
				t.Fatal("missing placement contract")
			}
			var accepted placement.Result
			for _, count := range []int{1, 2, 4, 8, 16} {
				names, memories := make([]string, count), make([]float64, count)
				for i := range names {
					names[i], memories[i] = "ZW-M890P", 144
				}
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
					Count: count, Vendor: "ppu", Names: names, MemoriesGiB: memories, MinMemoryGiB: 144,
				})
				if err != nil {
					t.Fatal(err)
				}
				if (result.Reason == "") != (count == 8) {
					t.Fatalf("%d cards: rejection = %q; only eight cards must be accepted", count, result.Reason)
				}
				if count == 8 {
					accepted = result
				}
			}
			for key, want := range map[string]int64{"GPU_COUNT": 8, "TP_SIZE": 8, "PP_SIZE": 1} {
				if accepted.Variables[key] != want {
					t.Errorf("%s = %v, want %d", key, accepted.Variables[key], want)
				}
			}
			input := m890pLaunchInput(t, spec, accepted)
			launch, err := deploy.ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
			for flag, value := range map[string]string{
				"--tp-size": "8", "--pp-size": "1", "--context-length": "262144",
				"--mem-fraction-static": "0.9", "--host": "0.0.0.0",
			} {
				if commandFlagCount(command, flag) != 1 || !containsArgumentPair(command, flag, value) {
					t.Errorf("want exactly one %s %s in %v", flag, value, command)
				}
			}
			for _, flag := range []string{"--trust-remote-code", "--enable-metrics", "--enable-cache-report"} {
				if commandFlagCount(command, flag) != 1 {
					t.Errorf("want exactly one %s in %v", flag, command)
				}
			}
			if commandFlagCount(command, "--quantization") != 0 {
				t.Errorf("checkpoint loading must not introduce a quantization override: %v", command)
			}
			for _, key := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
				if launch.Env[key] != "1" {
					t.Errorf("launch %s = %q, want 1", key, launch.Env[key])
				}
			}
			for _, test := range []struct {
				name            string
				assigned, nodes int
			}{
				{"only four assigned cards", 4, 1},
				{"two nodes would require sixteen cards", 8, 2},
			} {
				t.Run(test.name, func(t *testing.T) {
					invalid := input
					invalid.Params.AssignedGPUs, invalid.Params.NNodes = test.assigned, test.nodes
					_, err := deploy.ResolveLaunch(invalid)
					if err == nil || !strings.Contains(err.Error(), "but placement assigned") {
						t.Fatalf("want assigned world-size rejection, got %v", err)
					}
				})
			}
		})
	}
}

func m890pLaunchInput(t *testing.T, spec inferencecatalog.InferenceSpec, resolved placement.Result) deploy.ResolveLaunchInput {
	t.Helper()
	materialized, err := deploy.Materialize(deploy.MaterializeInput{
		Engine: spec.Engine, TemplateArgs: spec.Args, EngineVersion: "0.5.13",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	return deploy.ResolveLaunchInput{
		Engine: engine,
		Params: deploy.Params{
			ModelPath: "/model", ServedName: "operator-model", APIPort: 30000,
			AssignedGPUs: 8, TensorParallel: 8, PipelineParallel: 1,
			Quick: materialized.Quick, TemplateArgs: materialized.TemplateResidual, TemplateEnv: spec.Env,
		},
		PlacementVariables: resolved.Variables, APIContract: spec.API,
	}
}
