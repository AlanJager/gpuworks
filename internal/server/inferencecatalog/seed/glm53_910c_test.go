package seed

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

// Test the catalog-to-launch contract: a smaller device set must select its
// own recipe, and the generic allocator's device count must not become vLLM TP.
func TestGLM53AscendRecipesPlaceAndLaunchTheirOwnParallelism(t *testing.T) {
	for _, recipe := range []struct {
		name                     string
		count, tp, dp, sequences int
	}{
		{"w8a8-vllm-910c-stock-graph-16npu", 16, 2, 8, 4},
		{"w8a8-vllm-910c-stock-graph-8npu", 8, 4, 2, 4},
	} {
		t.Run(recipe.name, func(t *testing.T) {
			_, spec := ppuSpec(t, "gpuworks/glm-5.3-flash-910c", recipe.name)
			for _, count := range []int{1, 2, 4, 8, 16, 32} {
				resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
					Count: count, Vendor: "ascend", MinMemoryGiB: 64,
				})
				if err != nil {
					t.Fatal(err)
				}
				if (resolved.Reason == "") != (count == recipe.count) {
					t.Fatalf("%d devices: reason=%q", count, resolved.Reason)
				}
				if count != recipe.count {
					continue
				}
				engine, err := deploy.SelectEngine(spec.Engine)
				if err != nil {
					t.Fatal(err)
				}
				launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
					Engine: engine, Entrypoint: spec.Entrypoint, APIContract: spec.API, PlacementVariables: resolved.Variables,
					Params: deploy.Params{
						ModelPath: "/model", APIPort: 31417, NNodes: 1,
						AssignedGPUs: count, TensorParallel: count,
						TemplateArgs: spec.Args, TemplateEnv: spec.Env,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				for flag, want := range map[string]string{
					"--tensor-parallel-size":     strconv.Itoa(recipe.tp),
					"--data-parallel-size":       strconv.Itoa(recipe.dp),
					"--data-parallel-size-local": strconv.Itoa(recipe.dp),
					"--max-num-seqs":             strconv.Itoa(recipe.sequences),
					"--max-model-len":            "139264", "--port": "31417", "--kv-cache-dtype": "int8",
				} {
					got, found := deploy.LaunchFlag(launch.Argv, flag)
					if !found || got != want {
						t.Errorf("%s=%q, want %q", flag, got, want)
					}
				}
				if len(launch.Entrypoint) != 4 || launch.Entrypoint[0] != "/bin/bash" || !strings.Contains(launch.Entrypoint[2], `exec "$@"`) || len(launch.Argv) < 3 || launch.Argv[0] != "vllm" || launch.Argv[1] != "serve" || launch.Argv[2] != "/model" {
					t.Fatalf("direct engine/model arguments: %v", launch.Argv)
				}
				graph, _ := deploy.LaunchFlag(launch.Argv, "--compilation-config")
				var compilation struct {
					Mode  string `json:"cudagraph_mode"`
					Sizes []int  `json:"cudagraph_capture_sizes"`
				}
				if err := json.Unmarshal([]byte(graph), &compilation); err != nil || compilation.Mode != "FULL_DECODE_ONLY" || len(compilation.Sizes) == 0 {
					t.Fatalf("decode graph missing or invalid: %s", graph)
				}
				// Shared-expert DP requires TP-aligned graph sizes.
				// Small live batches must still fit a captured graph after padding.
				for _, size := range compilation.Sizes {
					if size < recipe.tp || size%recipe.tp != 0 {
						t.Fatalf("graph size %d is not aligned with TP%d", size, recipe.tp)
					}
					// Without speculative tokens, larger sizes are excluded from
					// decode capture by vLLM's scheduler limit.
					if size > recipe.sequences {
						t.Fatalf("graph size %d exceeds the scheduler limit %d", size, recipe.sequences)
					}
				}
				if compilation.Sizes[len(compilation.Sizes)-1] < recipe.sequences {
					t.Fatal("graph sizes do not cover the maximum live batch")
				}
				additional, _ := deploy.LaunchFlag(launch.Argv, "--additional-config")
				var optimizations struct {
					DSACP *bool `json:"enable_dsa_cp"`
				}
				// This image's DSA CP path corrupts the long-context known-answer
				// probe, including cold requests; keep the verified path explicit.
				if err := json.Unmarshal([]byte(additional), &optimizations); err != nil || optimizations.DSACP == nil || *optimizations.DSACP {
					t.Fatalf("DSA CP must be explicitly disabled: %s", additional)
				}
				for _, flag := range []string{"--enforce-eager", "--speculative-config"} {
					if _, found := deploy.LaunchFlag(launch.Argv, flag); found {
						t.Fatalf("unexpected runtime option: %s", flag)
					}
				}
				if len(spec.Assets) != 0 || len(spec.PreStartArgv) != 0 || spec.PreStartScript != "" || len(spec.Components) != 1 {
					t.Fatal("stock graph recipe must run without patch assets, launcher, or draft model")
				}
			}
			small, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: recipe.count, MinMemoryGiB: 32})
			if err != nil || small.Reason == "" {
				t.Fatalf("32 GiB devices must be refused: %+v, %v", small, err)
			}
		})
	}
}
