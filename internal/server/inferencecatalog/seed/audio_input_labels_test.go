package seed

import (
	"strings"
	"testing"
)

func TestCatalogInputLabelsSupportUILanguages(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.API == nil {
				continue
			}
			for _, operation := range spec.API.Operations {
				for _, input := range operation.Inputs {
					if input.Label == nil {
						if input.Kind == "audio" {
							t.Errorf("%s/%s/%s: audio input needs a template label", model.ModelID, spec.Name, input.Name)
						}
						continue
					}
					checked++
					for _, language := range []string{"en", "zh"} {
						if strings.TrimSpace(input.Label[language]) == "" {
							t.Errorf("%s/%s/%s: input label needs nonempty text for %s", model.ModelID, spec.Name, input.Name, language)
						}
					}
					for language := range input.Label {
						if language != "en" && language != "zh" {
							t.Errorf("%s/%s/%s: unsupported input label language %q; use en or zh", model.ModelID, spec.Name, input.Name, language)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the catalog contains no input labels to validate")
	}
}
