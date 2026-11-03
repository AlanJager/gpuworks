package seed

import (
	"slices"
	"testing"
)

func TestTranslateGemmaUsesOpenAIContentParts(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "google/translategemma-27b-it" {
			continue
		}
		found := 0
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" || spec.GPUs.CC != ">=8.0" {
				continue
			}
			found++
			index := slices.Index(spec.Args, "--chat-template-content-format")
			if index < 0 || index+1 >= len(spec.Args) || spec.Args[index+1] != "openai" {
				t.Fatalf("%s args=%v, want --chat-template-content-format openai", spec.Name, spec.Args)
			}
		}
		if found != 2 {
			t.Fatalf("NVIDIA TranslateGemma profiles=%d, want 2", found)
		}
		return
	}
	t.Fatal("TranslateGemma model not found")
}
