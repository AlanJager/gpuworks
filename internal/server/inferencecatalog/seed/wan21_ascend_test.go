package seed

import "testing"

func TestWan21A3OffersLatentAlignedVideoSizes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "wan-ai/wan2.1-t2v-1.3b-diffusers" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-sglang-ascend-a3-6f557629" || spec.API == nil {
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
					found = true
					if len(input.Options) != 2 || input.Options[0] != "640x352" || input.Options[1] != "1280x704" {
						t.Errorf("video size options = %v, want latent-aligned 640x352 and 1280x704", input.Options)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("Wan2.1 A3 video size input is missing")
	}
}
