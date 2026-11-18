package seed

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAscendSGLangStartupMemoryAdmission(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "startup.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, profile := range []struct{ model, spec string }{
		{"openbmb/minicpm-o-2_6", "bf16-sglang-ascend-5ea2ffbf"},
		{"openbmb/minicpm-o-2_6", "bf16-sglang-ascend-a3-5ea2ffbf"},
		{"inclusionai/ling-lite", "bf16-sglang-ascend-386736ff"},
		{"inclusionai/ling-lite", "bf16-sglang-ascend-a3-386736ff"},
	} {
		t.Run(profile.spec, func(t *testing.T) {
			model, spec := catalogSpec(t, profile.model, profile.spec)
			meta, err := json.Marshal(model.ModelConfig)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				memoryGiB int64
				indexes   []int
				wantCount int
			}{
				{32, []int{0}, 0},
				{32, []int{0, 1}, 2},
				{32, nil, 2},
				{64, []int{0}, 1},
			} {
				t.Run(fmt.Sprintf("%dGiB-selected%v", tc.memoryGiB, tc.indexes), func(t *testing.T) {
					node := &store.Node{UUID: "ascend-startup", GPUVendor: "ascend"}
					for index := range 2 {
						node.GPUs = append(node.GPUs, store.GPU{Index: index, Vendor: "ascend", Name: "910B4", MemoryTotal: tc.memoryGiB << 30})
					}
					paths, err := json.Marshal([]proto.PathUsage{{Kind: "models", AvailableBytes: 1 << 40}})
					if err != nil {
						t.Fatal(err)
					}
					node.PathUsage = string(paths)
					out := (planning.Input{
						Model: &store.Model{Name: model.ModelID, Meta: string(meta), WeightBytes: spec.WeightBytes},
						Node:  node, Engine: spec.Engine, GPUIndexes: tc.indexes,
						Quick:             deploy.LaunchConfig{GPUMemUtil: 0.9, Dtype: "bfloat16"},
						PlacementResolver: spec.Placement.Resolver,
					}).Evaluate(st)
					if out.Result.OK != (tc.wantCount > 0) {
						t.Fatalf("admitted=%v, want %d GPUs: %+v", out.Result.OK, tc.wantCount, out.Result)
					}
					if tc.wantCount > 0 && (out.Result.Plan == nil || out.Result.Plan.TP != tc.wantCount || out.Placement.Variables["TP_SIZE"] != int64(tc.wantCount)) {
						t.Fatalf("want TP%d, got plan=%+v placement=%+v", tc.wantCount, out.Result.Plan, out.Placement)
					}
				})
			}
		})
	}
}
