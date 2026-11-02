package deploy_test

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

// The A3 Qwen3-14B-GGUF template names one file inside the mounted
// repository; it must follow a custom model mount instead of pointing at the
// default /model.
func TestQwen314BGGUFA3ModelFileFollowsTheModelMount(t *testing.T) {
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "qwen/qwen3-14b-gguf" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "fp16-sglang-ascend-a3-41226ec8" {
				continue
			}
			engine, err := deploy.SelectEngine(spec.Engine)
			if err != nil {
				t.Fatal(err)
			}
			for _, mount := range []string{"", "/weights"} {
				want := "/model/Qwen3-14B-Q4_K_M.gguf"
				modelPath := "/model"
				if mount != "" {
					want = mount + "/Qwen3-14B-Q4_K_M.gguf"
					modelPath = mount
				}
				launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
					Engine: engine,
					Image:  spec.Image,
					Params: deploy.Params{
						ModelPath: modelPath, APIPort: 30001, AssignedGPUs: 2, TensorParallel: 2,
						TemplateArgs: spec.Args, TemplateEnv: spec.Env,
					},
					ContainerOverrides: deploy.ContainerOverrides{ModelMountPath: mount},
				})
				if err != nil {
					t.Fatalf("mount %q: %v", mount, err)
				}
				argv := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
				paths := 0
				for index, arg := range argv {
					if arg != "--model-path" {
						continue
					}
					paths++
					if index+1 >= len(argv) || argv[index+1] != want {
						t.Errorf("mount %q: --model-path = %q, want %q", mount, argv[min(index+1, len(argv)-1)], want)
					}
				}
				if paths != 1 {
					t.Errorf("mount %q: argv has %d --model-path flags: %q", mount, paths, argv)
				}
				if !slices.Contains(argv, "--disable-cuda-graph") {
					t.Errorf("mount %q: eager flag missing from %q", mount, argv)
				}
			}
			return
		}
	}
	t.Fatal("Qwen3-14B-GGUF A3 template is missing")
}
