package seed

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestMossAudioModelsMountTheirTokenizerOffline(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]string{
		"openmoss-team/moss-soundeffect":    "/models/MOSS-SoundEffect",
		"openmoss-team/moss-tts":            "/models/MOSS-TTS",
		"openmoss-team/moss-tts-realtime":   "/models/MOSS-TTS-Realtime",
		"openmoss-team/moss-ttsd-v1.0":      "/models/MOSS-TTSD-v1.0",
		"openmoss-team/moss-voicegenerator": "/models/MOSS-VoiceGenerator",
	}
	for _, model := range catalog.Models {
		primaryMount, ok := wanted[model.ModelID]
		if !ok {
			continue
		}
		delete(wanted, model.ModelID)
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			if len(spec.Components) != 2 {
				t.Fatalf("%s has %d components, want primary + audio tokenizer", model.ModelID, len(spec.Components))
			}
			primary := spec.Components[0]
			if primary.Role != "primary" || primary.MountPath != primaryMount {
				t.Errorf("%s primary component = %+v, want mount %s", model.ModelID, primary, primaryMount)
			}
			tokenizer := spec.Components[1]
			if tokenizer.Role != "auxiliary" ||
				tokenizer.MountPath != "/models/moss-audio-tokenizer" ||
				tokenizer.Weights.Sources[0].RepoID != "OpenMOSS-Team/MOSS-Audio-Tokenizer" {
				t.Errorf("%s audio tokenizer component = %+v", model.ModelID, tokenizer)
			}
			if model.ModelID != "openmoss-team/moss-tts-realtime" &&
				(!strings.Contains(spec.PreStartScript, "processing_moss_tts.py") ||
					!strings.Contains(spec.PreStartScript, tokenizer.MountPath)) {
				t.Errorf("%s request-time processor can still fetch the tokenizer online", model.ModelID)
			}
			args := strings.Join(spec.Args, " ")
			if !strings.Contains(args, tokenizer.MountPath) ||
				!strings.Contains(args, "codec_model_name_or_path") {
				t.Errorf("%s does not configure the mounted tokenizer in its launch contract: %v", model.ModelID, spec.Args)
			}
			if model.ModelID == "openmoss-team/moss-voicegenerator" {
				required := map[string]bool{
					"instructions": false,
					"ref_audio":    false,
				}
				for _, operation := range spec.API.Operations {
					for _, input := range operation.Inputs {
						if input.Required {
							if _, ok := required[input.Name]; ok {
								required[input.Name] = true
							}
						}
					}
				}
				for name, present := range required {
					if !present {
						t.Errorf("%s does not declare required %s input", model.ModelID, name)
					}
				}
				audioModality := false
				for _, modality := range model.InputModalities {
					audioModality = audioModality || modality == "audio"
				}
				if !audioModality {
					t.Errorf("%s does not advertise the required audio modality", model.ModelID)
				}
			}
		}
	}
	for modelID := range wanted {
		t.Errorf("%s is missing from the catalog", modelID)
	}
}

func TestGLMTTSCheckpointGetsOmniRootConfig(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "zai-org/glm-tts" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			for _, fragment := range []string{
				"/model/llm/config.json",
				"/tmp/glm-tts/config.json",
				`config["model_type"] = "glm_tts"`,
				`config["architectures"] = ["GLMTTSForConditionalGeneration"]`,
			} {
				if !strings.Contains(spec.PreStartScript, fragment) {
					t.Errorf("GLM-TTS preparation does not contain %q: %s", fragment, spec.PreStartScript)
				}
			}
			args := strings.Join(spec.Args, " ")
			if !strings.Contains(args, "--hf-config-path /tmp/glm-tts/config.json") {
				t.Errorf("GLM-TTS does not pass the prepared config to vLLM: %v", spec.Args)
			}
			return
		}
		t.Fatal("GLM-TTS NVIDIA spec is missing")
	}
	t.Fatal("GLM-TTS model is missing")
}

func TestLLaDAA100UsesTheCompatibleMoERouter(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{
		"inclusionai/llada2.1-flash": false,
		"inclusionai/llada2.1-mini":  false,
	}
	for _, model := range catalog.Models {
		if _, ok := wanted[model.ModelID]; !ok {
			continue
		}
		wanted[model.ModelID] = true
		for _, spec := range model.Specs {
			if spec.Name == "bf16-sglang-nvidia-general" &&
				spec.Env["SGLANG_OPT_USE_JIT_KERNEL_GROUPED_TOPK"] != "1" {
				t.Errorf("%s A100 profile selects the unsupported FlashInfer fused-top-k kernel", model.ModelID)
			}
			if spec.Name == "bf16-sglang-nvidia-general" {
				args := strings.Join(spec.Args, " ")
				if !strings.Contains(args, "--disable-cuda-graph") {
					t.Errorf("%s A100 profile must avoid oversized CUDA graph capture", model.ModelID)
				}
			}
		}
	}
	for modelID, seen := range wanted {
		if !seen {
			t.Errorf("%s is missing from the catalog", modelID)
		}
	}
}

func TestCosmosSyncVideoUsesItsMultipartRequestBody(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "nvidia/cosmos3-nano" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.API == nil {
				continue
			}
			for _, operation := range spec.API.Operations {
				if operation.Type == inferenceapi.OperationVideoGenerate &&
					operation.RequestEncoding != inferenceapi.EncodingMultipart {
					t.Errorf("%s declares %s for the sync video endpoint", spec.Name, operation.RequestEncoding)
				}
			}
		}
		return
	}
	t.Fatal("Cosmos3-Nano model is missing")
}

func TestLongCatImageEditExposesTheEditEndpoint(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "meituan-longcat/longcat-image-edit" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.API == nil {
				continue
			}
			for _, operation := range spec.API.Operations {
				if operation.Type == inferenceapi.OperationImageEdit {
					if operation.Endpoint != "/v1/images/edits" ||
						operation.RequestEncoding != inferenceapi.EncodingMultipart {
						t.Errorf("%s image edit operation = %+v", spec.Name, operation)
					}
					return
				}
			}
		}
		t.Fatal("LongCat image edit operation is missing")
	}
	t.Fatal("LongCat image edit model is missing")
}
