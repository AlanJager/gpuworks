package seed

import (
	"strings"
	"testing"
)

func TestMossTTSDMountsItsTokenizerAndLeavesRoomForTheCodec(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "openmoss-team/moss-ttsd-v1.0" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-nvidia-general" {
				continue
			}
			if len(spec.Components) != 2 {
				t.Fatalf("components = %d, want primary and audio tokenizer", len(spec.Components))
			}
			tokenizer := spec.Components[1]
			if tokenizer.Role != "auxiliary" ||
				tokenizer.MountPath != "/models/moss-audio-tokenizer" {
				t.Errorf("audio tokenizer component = %+v", tokenizer)
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
			} {
				if !strings.Contains(args, want) {
					t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
				}
			}
			return
		}
		t.Fatal("MOSS-TTSD NVIDIA spec is missing")
	}
	t.Fatal("MOSS-TTSD model is missing")
}
