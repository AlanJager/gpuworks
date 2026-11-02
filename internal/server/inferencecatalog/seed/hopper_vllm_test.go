package seed

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const hopperVLLMImage = "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2"

// The vLLM Hopper profiles of the two flash models. admits lists the card
// capacities (GiB) each profile must admit at 8 cards: H100 80 GB, H20 96 GB,
// and the 141 GB H20/H200.
var hopperVLLMProfiles = []struct {
	model, spec  string
	quantization string
	admits       []int64
	maxModelLen  string
}{
	{"zai-org/glm-5.3-flash", "fp8-vllm-nvidia-hopper-141g-tp8", "fp8", []int64{141}, ""},
	{"zai-org/glm-5.3-flash", "fp8-vllm-nvidia-hopper-80-96g-tp8", "fp8", []int64{80, 96}, "262144"},
	{"qwen/qwen3.8-flash-next", "bf16-vllm-nvidia-hopper-tp8", "bf16", []int64{80, 96, 141}, ""},
}

func TestHopperVLLMProfilesUseTheVerifiedRunnerAndMatchHopperCards(t *testing.T) {
	for _, profile := range hopperVLLMProfiles {
		t.Run(profile.model+"/"+profile.spec, func(t *testing.T) {
			_, spec := catalogSpec(t, profile.model, profile.spec)
			if spec.Engine != "vllm" || spec.Image != hopperVLLMImage || spec.Weights.Quantization != profile.quantization {
				t.Fatalf("runtime = %s / %s / %s", spec.Engine, spec.Image, spec.Weights.Quantization)
			}
			if spec.WeightBytes <= 0 || len(spec.Weights.Sources) != 1 || spec.Weights.Sources[0].Revision == "" {
				t.Fatalf("weights must be one pinned source with a size: %+v", spec.Weights)
			}
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
			for _, card := range []struct {
				name, vendor, cc string
				want             bool
			}{
				{"NVIDIA H200", "nvidia", "9.0", true},
				{"NVIDIA H20", "nvidia", "9.0", true},
				{"NVIDIA H20-3e", "nvidia", "9.0", true},
				{"NVIDIA H100 80GB HBM3", "nvidia", "9.0", true},
				{"NVIDIA H100 PCIe", "nvidia", "9.0", true},
				{"NVIDIA B200", "nvidia", "10.0", false},
				{"NVIDIA H800", "nvidia", "9.0", false},
				{"NVIDIA H1000", "nvidia", "9.0", false},
				{"NVIDIA H2080", "nvidia", "9.0", false},
				{"NVIDIA A100-SXM4-80GB", "nvidia", "8.0", false},
				{"H100", "amd", "", false},
			} {
				for _, name := range []string{card.name, accelerator.NormalizeGPUName(card.name)} {
					gpu := chipmatch.ReportedGPU(name, card.vendor, "", card.cc)
					if got := chipmatch.Evaluate(gpu, predicate).Matched; got != card.want {
						t.Errorf("%s %q matched=%v, want %v", card.vendor, name, got, card.want)
					}
				}
			}
		})
	}
}

// The launch arguments carry what was measured or what the recipe requires; a
// silent edit that drops one changes behavior on real hardware.
func TestHopperVLLMProfilesKeepTheirLaunchContract(t *testing.T) {
	for _, profile := range hopperVLLMProfiles {
		t.Run(profile.model+"/"+profile.spec, func(t *testing.T) {
			_, spec := catalogSpec(t, profile.model, profile.spec)
			has := func(flag string) bool { return slices.Contains(spec.Args, flag) }
			value := func(flag string) string {
				if i := slices.Index(spec.Args, flag); i >= 0 && i+1 < len(spec.Args) {
					return spec.Args[i+1]
				}
				return ""
			}
			if !has("--enable-auto-tool-choice") || value("--tool-call-parser") == "" || value("--reasoning-parser") == "" {
				t.Errorf("tool/reasoning parsers missing: %v", spec.Args)
			}
			if got := value("--max-model-len"); got != profile.maxModelLen {
				t.Errorf("--max-model-len = %q, want %q", got, profile.maxModelLen)
			}
			if spec.Env["VLLM_ENGINE_READY_TIMEOUT_S"] == "" {
				t.Error("cold start (weight load, kernel compile) outlasts vLLM's default ready timeout")
			}
			isQwen := profile.model == "qwen/qwen3.8-flash-next"
			// Measured on 8x H20 141 GB with sleep mode on: without it the FULL
			// CUDA graph capture fails in custom_all_reduce.cuh.
			if has("--disable-custom-all-reduce") != isQwen {
				t.Errorf("--disable-custom-all-reduce = %v, want %v", has("--disable-custom-all-reduce"), isQwen)
			}
			if isQwen && (has("--enable-expert-parallel") || value("--max-num-seqs") != "256") {
				t.Errorf("Qwen BF16 profile drifted from the run on hardware: %v", spec.Args)
			}
		})
	}
}

// Synthetic idle nodes exercise the catalog's admission rules, not engine
// startup: only 8 cards, and only the capacities each profile is laid out for.
func TestHopperVLLMProfilesAdmitOnlyEightCardsOfTheirCapacity(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hopper.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, profile := range hopperVLLMProfiles {
		t.Run(profile.model+"/"+profile.spec, func(t *testing.T) {
			model, spec := catalogSpec(t, profile.model, profile.spec)
			meta, err := json.Marshal(model.ModelConfig)
			if err != nil {
				t.Fatal(err)
			}
			for _, card := range []struct {
				name string
				gib  int64
			}{{"NVIDIA H100 80GB HBM3", 80}, {"NVIDIA H20", 96}, {"NVIDIA H20", 141}, {"NVIDIA H200", 141}} {
				for _, count := range []int{4, 8} {
					t.Run(fmt.Sprintf("%dx%s-%dGiB", count, card.name, card.gib), func(t *testing.T) {
						node := &store.Node{UUID: "hopper", GPUVendor: "nvidia"}
						indexes := make([]int, count)
						cards := make([]deploy.CardFacts, count)
						// nvidia-smi reports a little under the marketing size.
						total := card.gib<<30 - 1<<29
						for index := range count {
							indexes[index] = index
							node.GPUs = append(node.GPUs, store.GPU{Index: index, Name: card.name, Vendor: "nvidia",
								ComputeCapability: "9.0", MemoryTotal: total})
							cards[index] = deploy.CardFacts{Name: card.name, MemoryTotalBytes: total}
						}
						paths, err := json.Marshal([]proto.PathUsage{{Kind: "models", AvailableBytes: 2 << 40}})
						if err != nil {
							t.Fatal(err)
						}
						node.PathUsage = string(paths)
						materialized, err := deploy.Materialize(deploy.MaterializeInput{Engine: spec.Engine, TemplateArgs: spec.Args, Cards: cards})
						if err != nil {
							t.Fatal(err)
						}
						out := (planning.Input{
							Model: &store.Model{Name: model.ModelID, Meta: string(meta), WeightBytes: spec.WeightBytes},
							Node:  node, Engine: spec.Engine, Quick: materialized.Quick, GPUIndexes: indexes,
							RuntimeWeightBytes: spec.RuntimeWeightBytes, PlacementResolver: spec.Placement.Resolver,
						}).Evaluate(st)
						want := count == 8 && slices.Contains(profile.admits, card.gib)
						if out.Result.OK != want {
							t.Fatalf("admitted=%v, want %v: %+v; placement error=%v", out.Result.OK, want, out.Result, out.PlacementError)
						}
						if want && out.Placement.Variables["TP_SIZE"] != int64(8) {
							t.Fatalf("TP_SIZE = %v, want 8", out.Placement.Variables["TP_SIZE"])
						}
					})
				}
			}
		})
	}
}
