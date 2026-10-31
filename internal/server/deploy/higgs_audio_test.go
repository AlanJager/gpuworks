package deploy_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func TestHiggsReferenceAudioLaunchRetainsOfflineEnvironment(t *testing.T) {
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "bosonai/higgs-audio-v3-tts-4b" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			engine, err := deploy.SelectEngine(spec.Engine)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine: engine,
				Image:  spec.Image,
				Params: deploy.Params{
					ModelPath: "/model", APIPort: 30001, AssignedGPUs: 1, TensorParallel: 1,
					TemplateArgs: spec.Args, TemplateEnv: spec.Env,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"} {
				if launch.Env[name] != "1" {
					t.Errorf("%s = %q: the mounted reference-audio encoder must not require runtime downloads", name, launch.Env[name])
				}
			}
			if launch.Env["HIGGS_AUDIO_TOKENIZER_PATH"] != "/models/audio-tokenizer" {
				t.Errorf("resolved encoder path = %q", launch.Env["HIGGS_AUDIO_TOKENIZER_PATH"])
			}
			return
		}
	}
	t.Fatal("NVIDIA Higgs reference-audio template is missing")
}
