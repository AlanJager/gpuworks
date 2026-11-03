package seed

import (
	"strings"
	"testing"
)

func TestMossTTSStartsWithBothStagesOnOneA100(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "openmoss-team/moss-tts" {
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
			return
		}
		t.Fatal("MOSS-TTS NVIDIA spec is missing")
	}
	t.Fatal("MOSS-TTS model is missing")
}
