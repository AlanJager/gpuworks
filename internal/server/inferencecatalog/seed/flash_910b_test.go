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

// The 910B (A2) profiles for Qwen3.8-Flash-Next and GLM-5.3-Flash: each takes
// only 64 GiB A2 cards in its own device count, launches that count as vLLM TP,
// and the MTP profiles are the only ones that turn on speculative decoding.
func TestFlash910BProfilesPlaceAndLaunchTheirOwnShape(t *testing.T) {
	for _, recipe := range []struct {
		model, name string
		count       int
		mtp         bool
	}{
		{"qwen/qwen3.8-flash-next", "int8-vllm-910b-tp4", 4, false},
		{"qwen/qwen3.8-flash-next", "int8-vllm-910b-tp4-mtp", 4, true},
		{"qwen/qwen3.8-flash-next", "w4a8-vllm-910b-tp4", 4, false},
		{"qwen/qwen3.8-flash-next", "w4a8-vllm-910b-tp4-mtp", 4, true},
		{"zai-org/glm-5.3-flash", "w8a8-vllm-910b-graph-tp8", 8, false},
	} {
		t.Run(recipe.name, func(t *testing.T) {
			_, spec := catalogSpec(t, recipe.model, recipe.name)
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name}
			for name, want := range map[string]bool{
				"Ascend 910B3": true, "Ascend 910B4": true, "Ascend 910B4-1": true, "Ascend 910B2C": true,
				"Ascend 910C": false, "Ascend 310P3": false,
			} {
				if got := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate).Matched; got != want {
					t.Fatalf("card %q matched=%t, want %t", name, got, want)
				}
			}
			small, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: recipe.count, Vendor: "ascend", MinMemoryGiB: 32})
			if err != nil {
				t.Fatal(err)
			}
			if small.Reason == "" {
				t.Fatal("32 GiB cards were accepted")
			}
			for _, count := range []int{1, 2, 4, 8, 16} {
				resolved, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: count, Vendor: "ascend", MinMemoryGiB: 64})
				if err != nil {
					t.Fatal(err)
				}
				if (resolved.Reason == "") != (count == recipe.count) {
					t.Fatalf("%d cards: reason=%q", count, resolved.Reason)
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
						ModelPath: "/model", ServedName: "flash-910b", APIPort: 31417, NNodes: 1,
						RuntimeDriver: "udocker",
						AssignedGPUs:  count, TensorParallel: count,
						TemplateArgs: spec.Args, TemplateEnv: spec.Env,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				for flag, want := range map[string]string{
					"--tensor-parallel-size": strconv.Itoa(recipe.count), "--port": "31417",
					"--served-model-name": "flash-910b", "--quantization": "ascend",
				} {
					if got, found := deploy.LaunchFlag(launch.Argv, flag); !found || got != want {
						t.Errorf("%s=%q, want %q", flag, got, want)
					}
				}
				speculative, found := deploy.LaunchFlag(launch.Argv, "--speculative-config")
				if found != recipe.mtp {
					t.Fatalf("--speculative-config present=%t, want %t", found, recipe.mtp)
				}
				if recipe.mtp {
					var config struct {
						Method string `json:"method"`
						Tokens int    `json:"num_speculative_tokens"`
					}
					if err := json.Unmarshal([]byte(speculative), &config); err != nil || config.Method != "mtp" || config.Tokens != 3 {
						t.Fatalf("speculative config %q (%v)", speculative, err)
					}
					// Every captured decode graph must hold whole sequences of one
					// target token plus the draft tokens.
					graphs, _ := deploy.LaunchFlag(launch.Argv, "--compilation-config")
					var compilation struct {
						Sizes []int `json:"cudagraph_capture_sizes"`
					}
					if err := json.Unmarshal([]byte(graphs), &compilation); err != nil || len(compilation.Sizes) == 0 {
						t.Fatalf("compilation config %q (%v)", graphs, err)
					}
					for _, size := range compilation.Sizes {
						if size%(config.Tokens+1) != 0 {
							t.Errorf("graph size %d is not a multiple of %d", size, config.Tokens+1)
						}
					}
				}
				if recipe.model == "zai-org/glm-5.3-flash" {
					// serve.py derives --hf-overrides from the mounted checkpoint and
					// refuses a second copy; the entrypoint runs it, not vllm directly.
					if _, found := deploy.LaunchFlag(launch.Argv, "--hf-overrides"); found {
						t.Fatal("the GLM 910B profile must not pass --hf-overrides itself")
					}
					if len(launch.Entrypoint) != 4 || !strings.HasSuffix(launch.Entrypoint[2], `exec python /opt/gpuworks/glm53-910b/serve.py "$@"`) {
						t.Fatalf("entrypoint %q", launch.Entrypoint)
					}
					// serve.py reads the model path from its first argument.
					if len(launch.Argv) == 0 || launch.Argv[0] != "/model" {
						t.Fatalf("argv does not start with the model path: %q", launch.Argv)
					}
				}
			}
		})
	}
}
