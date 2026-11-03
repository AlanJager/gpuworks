package seed

import (
	"strings"
	"testing"
)

func TestMossVoiceGeneratorStartsWithBothStagesOnOneA100(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "openmoss-team/moss-voicegenerator" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			if len(spec.Components) != 2 ||
				spec.Components[1].MountPath != "/models/moss-audio-tokenizer" {
				t.Fatalf("components = %+v, want primary and audio tokenizer", spec.Components)
			}
			removeAt := strings.Index(spec.PreStartScript, `rm -f "$dst/processing_moss_tts.py"`)
			copyAt := strings.Index(spec.PreStartScript, `cp "$src/processing_moss_tts.py" "$dst/processing_moss_tts.py"`)
			if removeAt < 0 || copyAt < 0 || removeAt > copyAt {
				t.Errorf("preparation must remove the linked processor before copying it: %s", spec.PreStartScript)
			}
			args := strings.Join(spec.Args, " ")
			for _, want := range []string{
				`codec_model_name_or_path":"/models/moss-audio-tokenizer`,
				"--gpu-memory-utilization 0.75",
				"--enforce-eager",
			} {
				if !strings.Contains(args, want) {
					t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
				}
			}
			required := map[string]bool{"instructions": false, "ref_audio": false}
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
					t.Errorf("operation does not declare required %s input", name)
				}
			}
			return
		}
		t.Fatal("MOSS-VoiceGenerator NVIDIA spec is missing")
	}
	t.Fatal("MOSS-VoiceGenerator model is missing")
}
