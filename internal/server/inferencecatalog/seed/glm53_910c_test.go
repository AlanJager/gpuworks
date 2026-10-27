package seed

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

// Test the catalog-to-launch contract: a smaller device set must select its
// own recipe, and the generic allocator's device count must not become vLLM TP.
func TestGLM53AscendRecipesPlaceAndLaunchTheirOwnParallelism(t *testing.T) {
	for _, recipe := range []struct {
		name                     string
		count, tp, dp, sequences int
		draftTokens              int
	}{
		{"w8a8-vllm-910c-stock-graph-16npu", 16, 2, 8, 4, 0},
		{"w8a8-vllm-910c-stock-graph-8npu", 8, 4, 2, 4, 0},
		{"w8a8-vllm-910c-dflash5-16npu", 16, 2, 8, 4, 5},
	} {
		t.Run(recipe.name, func(t *testing.T) {
			_, spec := ppuSpec(t, "gpuworks/glm-5.3-flash-910c", recipe.name)
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name}
			for _, name := range []string{"Ascend 910C", "Ascend 910C3", "Ascend 910B1", "Ascend 910B2C", "Ascend 310P"} {
				want := strings.Contains(name, "910C")
				if got := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate).Matched; got != want {
					t.Fatalf("card %q matched=%t, want %t", name, got, want)
				}
			}
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
					Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint, APIContract: spec.API, PlacementVariables: resolved.Variables,
					Params: deploy.Params{
						ModelPath: "/model", ServedName: "GLM-5.3-Flash-W8A8-tdxi", APIPort: 31417, NNodes: 1,
						RuntimeDriver: "docker",
						AssignedGPUs:  count, TensorParallel: count,
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
					// The gateway sends a callable engine the served name as is.
					"--served-model-name": "GLM-5.3-Flash-W8A8-tdxi",
				} {
					got, found := deploy.LaunchFlag(launch.Argv, flag)
					if !found || got != want {
						t.Errorf("%s=%q, want %q", flag, got, want)
					}
				}
				if recipe.draftTokens == 0 {
					if len(launch.Entrypoint) != 4 || launch.Entrypoint[0] != "/bin/bash" || !strings.Contains(launch.Entrypoint[2], `exec "$@"`) || len(launch.Argv) < 3 || launch.Argv[0] != "vllm" || launch.Argv[1] != "serve" || launch.Argv[2] != "/model" {
						t.Fatalf("direct engine/model arguments: %v", launch.Argv)
					}
				} else {
					if strings.Join(launch.Entrypoint, " ") != "bash /opt/gpuworks/glm53-910c/serve.sh" || len(launch.Argv) < 5 || strings.Join(launch.Argv[:5], " ") != "1 16 2 serve /model" {
						t.Fatalf("DFlash launcher topology/model: %v %v", launch.Entrypoint, launch.Argv)
					}
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
					// Decode capture must include the speculative verification tokens.
					if size > recipe.sequences*(recipe.draftTokens+1) {
						t.Fatalf("graph size %d exceeds the scheduler limit %d", size, recipe.sequences)
					}
				}
				if compilation.Sizes[len(compilation.Sizes)-1] < recipe.sequences*(recipe.draftTokens+1) {
					t.Fatal("graph sizes do not cover the maximum live batch")
				}
				additional, _ := deploy.LaunchFlag(launch.Argv, "--additional-config")
				var optimizations struct {
					DSACP    *bool `json:"enable_dsa_cp"`
					FusedMC2 int   `json:"enable_fused_mc2"`
				}
				// The DFlash overlay includes the DSA/cache adaptations; the
				// stock image requires DSA CP disabled for correct long prompts.
				if err := json.Unmarshal([]byte(additional), &optimizations); err != nil || optimizations.FusedMC2 != 1 || optimizations.DSACP == nil || *optimizations.DSACP != (recipe.draftTokens > 0) {
					t.Fatalf("unexpected DSA CP setting: %s", additional)
				}
				if _, found := deploy.LaunchFlag(launch.Argv, "--enforce-eager"); found {
					t.Fatal("decode graph disabled")
				}
				speculative, hasDraft := deploy.LaunchFlag(launch.Argv, "--speculative-config")
				if recipe.draftTokens == 0 {
					if hasDraft || len(spec.Assets) != 0 || len(spec.PreStartArgv) != 0 || spec.PreStartScript != "" || len(spec.Components) != 1 {
						t.Fatal("stock graph recipe must run without patch assets, launcher, or draft model")
					}
					if len(launch.PreStartArgv) != 0 || launch.PreStartScript != "" || len(launch.RuntimeAssets) != 0 {
						t.Fatal("resolved stock launch must not inject runtime patches")
					}
				} else {
					var draft struct {
						Method string `json:"method"`
						Model  string `json:"model"`
						Tokens int    `json:"num_speculative_tokens"`
						Eager  bool   `json:"enforce_eager"`
					}
					if !hasDraft || json.Unmarshal([]byte(speculative), &draft) != nil || draft.Method != "dflash" || draft.Model != "/draft" || draft.Tokens != 5 || draft.Eager {
						t.Fatalf("invalid DFlash config: %s", speculative)
					}
					if len(spec.Assets) != 1 || spec.Assets[0].UnpackTo != "/opt/gpuworks/glm53-910c" || len(spec.Components) != 2 || spec.Components[1].MountPath != draft.Model || spec.Components[1].Role != "speculative_draft" {
						t.Fatal("DFlash runtime asset and draft mount must match the launcher")
					}
				}
			}
			small, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: recipe.count, MinMemoryGiB: 32})
			if err != nil || small.Reason == "" {
				t.Fatalf("32 GiB devices must be refused: %+v, %v", small, err)
			}
		})
	}
}
