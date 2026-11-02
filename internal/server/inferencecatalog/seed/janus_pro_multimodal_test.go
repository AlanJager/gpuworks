package seed

import (
	"slices"
	"testing"
)

func TestJanusProModelsDeclareImageInput(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{
		"deepseek-ai/janus-pro-1b": false,
		"deepseek-ai/janus-pro-7b": false,
	}
	for _, model := range catalog.Models {
		if _, ok := found[model.ModelID]; !ok {
			continue
		}
		if !slices.Equal(model.InputModalities, []string{"text", "image"}) {
			t.Errorf("%s input modalities = %v, want text and image", model.ModelID, model.InputModalities)
		}
		found[model.ModelID] = true
	}
	for modelID, ok := range found {
		if !ok {
			t.Errorf("%s is missing", modelID)
		}
	}
}
