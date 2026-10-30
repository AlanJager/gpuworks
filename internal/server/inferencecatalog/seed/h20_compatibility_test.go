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
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var h20CatalogProfiles = []struct {
	model, spec    string
	counts96       []int
	counts141      []int
	allowBlackwell bool
}{
	{"deepseek-ai/deepseek-v4.1-flash", "fp4-sglang-nvidia-h200-tp8", []int{8}, []int{8}, false},
	{"deepseek-ai/deepseek-v4.1-flash-1017", "fp4-sglang-nvidia-h200-tp8", []int{8}, []int{8}, false},
	{"deepseek-ai/deepseek-v4.1-flash-1017", "fp4-sglang-nvidia-h200-tp8-throughput", []int{8}, []int{8}, false},
	{"deepseek-ai/deepseek-v4.1-flash-1017", "fp4-sglang-nvidia-h200-tp4-engram-host", nil, []int{4}, false},
	{"deepseek-ai/deepseek-v4.1-flash-1017", "fp4-sglang-nvidia-h200-tp4-engram-host-dspark", nil, []int{4}, false},
	{"deepseek-ai/deepseek-v4.1-flash-1017", "fp4-vllm-nvidia-h200-tp4-engram-host", nil, []int{4}, false},
	{"qwen/qwen3.8-flash-next", "fp8-sglang-nvidia-tp4", []int{4}, []int{4}, false},
	{"zai-org/glm-5.3-flash", "fp8-sglang-nvidia-h200", []int{8}, []int{4, 8}, false},
	{"zai-org/glm-5.3", "fp8-vllm-nvidia-tp8", nil, []int{8}, true},
}

func TestH20CatalogProfilesMatchSupportedNames(t *testing.T) {
	for _, profile := range h20CatalogProfiles {
		t.Run(profile.model+"/"+profile.spec, func(t *testing.T) {
			_, spec := ppuSpec(t, profile.model, profile.spec)
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
			for _, card := range []struct {
				name, vendor, cc string
				want             bool
			}{
				{"NVIDIA H20", "nvidia", "9.0", true},
				{"H20", "nvidia", "9.0", true},
				{"NVIDIA H20-3e", "nvidia", "9.0", true},
				{"NVIDIA H200", "nvidia", "9.0", true},
				{"NVIDIA B200", "nvidia", "10.0", profile.allowBlackwell},
				{"NVIDIA B300", "nvidia", "10.3", profile.allowBlackwell},
				{"NVIDIA H100", "nvidia", "9.0", false},
				{"NVIDIA A100-SXM4-80GB", "nvidia", "8.0", false},
				{"NVIDIA H2080", "nvidia", "9.0", false},
				{"NVIDIA H2000", "nvidia", "9.0", false},
				{"H20", "amd", "", false},
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

// Synthetic idle nodes exercise the catalog's existing admission rules, not
// engine startup. Both capacities deliberately report the same H20 name.
func TestH20CatalogProfilesRespectCapacityAndCardCount(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "h20.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, profile := range h20CatalogProfiles {
		t.Run(profile.model+"/"+profile.spec, func(t *testing.T) {
			model, spec := ppuSpec(t, profile.model, profile.spec)
			meta, err := json.Marshal(model.ModelConfig)
			if err != nil {
				t.Fatal(err)
			}
			predicate := chipmatch.Spec{Name: spec.Name, Quantization: spec.Weights.Quantization,
				GPUs: chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}}
			if err := chipmatch.ValidateSpec(chipmatch.ReportedGPU("NVIDIA H20", "nvidia", "sm_90", "9.0"), predicate, nil); err != nil {
				t.Fatalf("H20 cannot reach resource admission: %v", err)
			}
			for _, capacity := range []struct {
				gib    int64
				counts []int
			}{{96, profile.counts96}, {141, profile.counts141}} {
				for _, count := range []int{2, 4, 8} {
					t.Run(fmt.Sprintf("%dx%dGiB", count, capacity.gib), func(t *testing.T) {
						node := &store.Node{UUID: "h20", GPUVendor: "nvidia"}
						indexes := make([]int, count)
						cards := make([]deploy.CardFacts, count)
						for index := range count {
							indexes[index] = index
							node.GPUs = append(node.GPUs, store.GPU{Index: index, Name: "NVIDIA H20", Vendor: "nvidia",
								ComputeCapability: "9.0", MemoryTotal: capacity.gib << 30})
							cards[index] = deploy.CardFacts{Name: "NVIDIA H20", MemoryTotalBytes: capacity.gib << 30}
						}
						paths, err := json.Marshal([]proto.PathUsage{{Kind: "models", AvailableBytes: 2 << 40}})
						if err != nil {
							t.Fatal(err)
						}
						node.PathUsage = string(paths)
						materialized, err := deploy.Materialize(deploy.MaterializeInput{
							Engine: spec.Engine, TemplateArgs: spec.Args, Cards: cards,
						})
						if err != nil {
							t.Fatal(err)
						}
						out := (planning.Input{
							Model: &store.Model{Name: model.ModelID, Meta: string(meta), WeightBytes: spec.WeightBytes},
							Node:  node, Engine: spec.Engine, Quick: materialized.Quick, GPUIndexes: indexes,
							RuntimeWeightBytes: spec.RuntimeWeightBytes, PlacementResolver: spec.Placement.Resolver,
						}).Evaluate(st)
						want := slices.Contains(capacity.counts, count)
						if out.Result.OK != want {
							t.Fatalf("admitted=%v, want %v: %+v; placement error=%v", out.Result.OK, want, out.Result, out.PlacementError)
						}
						if want {
							if out.Result.Plan == nil || len(out.Result.Plan.GPUIndexes) != count || out.Placement.Variables["TP_SIZE"] != int64(count) {
								t.Fatalf("explicit GPU selection changed: plan=%+v placement=%+v", out.Result.Plan, out.Placement)
							}
							return
						}
						wantReason := scheduler.ReasonPlacementRejected
						if slices.Contains(profile.counts141, count) {
							wantReason = scheduler.ReasonNoFit
						}
						if !slices.ContainsFunc(out.Result.Explain.Rejected, func(r scheduler.Rejected) bool { return r.Reason.Code == wantReason }) {
							t.Fatalf("want %s rejection, got %+v", wantReason, out.Result)
						}
					})
				}
			}
		})
	}
}
