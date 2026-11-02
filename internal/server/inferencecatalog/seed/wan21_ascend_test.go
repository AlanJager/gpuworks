package seed

import "testing"

func TestWan21A3OffersLatentAlignedVideoSizes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		if model.ModelID != "wan-ai/wan2.1-t2v-1.3b-diffusers" {
			continue
		}
		for _, spec := range model.Specs {
			if (spec.Name != "bf16-sglang-ascend-6f557629" && spec.Name != "bf16-sglang-ascend-a3-6f557629") || spec.API == nil {
				continue
			}
			for _, operation := range spec.API.Operations {
				if operation.Type != "openai.video.generate" {
					continue
				}
				for _, input := range operation.Inputs {
					if input.Name != "size" {
						continue
					}
					seen[spec.Name] = true
					if len(input.Options) != 2 || input.Options[0] != "640x352" || input.Options[1] != "1280x704" {
						t.Errorf("%s video size options = %v, want latent-aligned 640x352 and 1280x704", spec.Name, input.Options)
					}
				}
			}
		}
	}
	for _, specName := range []string{"bf16-sglang-ascend-6f557629", "bf16-sglang-ascend-a3-6f557629"} {
		if !seen[specName] {
			t.Errorf("Wan2.1 %s video size input is missing", specName)
		}
	}
}
