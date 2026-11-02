package seed

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestHiggsAudioV3ReferenceAudioLoadsItsEncoderLocally(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "bosonai/higgs-audio-v3-tts-4b" {
			continue
		}
		found := false
		for _, spec := range model.Specs {
			for _, source := range spec.Weights.Sources {
				if source.RepoID != "bosonai/higgs-tts-3-4b" {
					t.Errorf("%s must use the current Higgs repository name: %+v", spec.Name, source)
				}
			}
			if spec.API == nil {
				t.Fatalf("%s has no audio operation", spec.Name)
			}
			declared := false
			for _, operation := range spec.API.Operations {
				if operation.Type != inferenceapi.OperationAudioSpeech {
					continue
				}
				for _, input := range operation.Inputs {
					if input.Name == "ref_audio" {
						declared = input.Kind == "audio" && input.Encoding == "data_url"
					}
				}
			}
			if !declared {
				t.Errorf("%s must declare reference audio with its API name and data URL encoding", spec.Name)
			}
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			found = true
			if len(spec.Components) != 2 {
				t.Fatalf("reference-audio encoding must be acquired alongside the primary weights: %+v", spec.Components)
			}
			primary, encoder := spec.Components[0], spec.Components[1]
			if !reflect.DeepEqual(primary.Weights, spec.Weights) {
				t.Errorf("primary component acquisition must use the same pinned sources as model selection: %+v != %+v", primary.Weights, spec.Weights)
			}
			if primary.Role != "primary" || primary.MountPath != "/model" || primary.WeightBytes != spec.WeightBytes {
				t.Errorf("primary model mount must stay compatible with the engine: %+v", primary)
			}
			if encoder.Role != "auxiliary" || encoder.MountPath != "/models/audio-tokenizer" || encoder.WeightBytes != 805675058 {
				t.Errorf("reference-audio encoder is not a separate mounted dependency: %+v", encoder)
			}
			if spec.Env["HIGGS_AUDIO_TOKENIZER_PATH"] != encoder.MountPath {
				t.Errorf("voice cloning must resolve its local encoder mount: %v", spec.Env)
			}
			revisions := map[string]string{
				"huggingface": "18db15024ce4b7e15638be6ef0e283d99d282f39",
				"modelscope":  "316f89b819e04e49dfbd0239f85e4e363b862cce",
			}
			if len(encoder.Weights.Sources) != len(revisions) {
				t.Fatalf("encoder acquisition must work with either configured weight provider: %+v", encoder.Weights.Sources)
			}
			for _, source := range encoder.Weights.Sources {
				revision, ok := revisions[source.Provider]
				if !ok || source.RepoID != "k2-fsa/OmniVoice" || source.Revision != revision || source.Subfolder != "audio_tokenizer" {
					t.Errorf("download the same pinned codec subfolder from each provider: %+v", source)
				}
				delete(revisions, source.Provider)
			}
			if len(revisions) != 0 {
				t.Errorf("missing codec providers: %v", revisions)
			}
		}
		if !found {
			t.Fatal("NVIDIA Higgs Audio V3 template is missing")
		}
		return
	}
	t.Fatal("Higgs Audio V3 model is missing")
}
