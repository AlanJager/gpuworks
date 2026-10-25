package seed

import (
	"context"
	"strconv"
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
		{"w8a8-vllm-910c-dflash5-16npu", 16, 2, 8, 4},
		{"w8a8-vllm-910c-dflash5-8npu", 8, 4, 2, 2},
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
					Engine: engine,
					Params: deploy.Params{
						ModelPath: "/model", APIPort: 31417, NNodes: 1,
						AssignedGPUs: count, TensorParallel: recipe.tp,
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
				if len(launch.Argv) < 5 || launch.Argv[0] != "1" || launch.Argv[1] != strconv.Itoa(count) || launch.Argv[2] != strconv.Itoa(recipe.tp) || launch.Argv[4] != "/model" {
					t.Fatalf("launcher topology/model arguments: %v", launch.Argv)
				}
			}
			small, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: recipe.count, MinMemoryGiB: 32})
			if err != nil || small.Reason == "" {
				t.Fatalf("32 GiB devices must be refused: %+v, %v", small, err)
			}
		})
	}
}
