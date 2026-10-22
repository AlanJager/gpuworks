package inferencecatalog

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func editableRow(source string) store.InferenceModel {
	return store.InferenceModel{
		ModelID: "acme/edit", Source: source, DisplayTitle: "Edit", ReadmeZHCN: "README.zh-CN.md", ReadmeEN: "README.en.md",
		Specs: []store.InferenceSpecRow{
			{Source: "upstream", Name: "stock", Engine: "vllm", Image: "vllm:stock", Quantization: "bf16",
				WeightSources: `[{"provider":"huggingface","repo_id":"acme/edit"}]`, GPUs: `{"vendor":"nvidia"}`},
			{Source: "user", Name: "mine", Engine: "vllm", Image: "vllm:mine", Quantization: "bf16",
				WeightSources: `[{"provider":"huggingface","repo_id":"acme/edit","revision":"abc"}]`, GPUs: `{"vendor":"nvidia"}`},
		},
	}
}

func specNames(t *testing.T, document []byte) []string {
	t.Helper()
	catalog, err := DecodeYAML(document)
	if err != nil {
		t.Fatalf("editable YAML does not decode: %v\n%s", err, document)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].Source != "user" {
		t.Fatalf("want one source:user model, got %+v", catalog.Models)
	}
	var names []string
	for _, spec := range catalog.Models[0].Specs {
		names = append(names, spec.Name)
	}
	return names
}

func TestEditableYAMLSpecSourceFilterAndDocumentationOwnership(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		specSource string
		specs      []string
		readme     bool
	}{
		{"user model, every spec", "user", "", []string{"stock", "mine"}, true},
		{"user model, user specs", "user", "user", []string{"mine"}, true},
		{"upstream model, user specs", "upstream", "user", []string{"mine"}, false},
		{"upstream model, upstream specs", "upstream", "upstream", []string{"stock"}, false},
		{"upstream model, every spec", "upstream", "", []string{"stock", "mine"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document, err := EditableYAML(editableRow(tc.source), tc.specSource)
			if err != nil {
				t.Fatal(err)
			}
			if got := specNames(t, document); strings.Join(got, ",") != strings.Join(tc.specs, ",") {
				t.Fatalf("specs=%v want %v\n%s", got, tc.specs, document)
			}
			if has := strings.Contains(string(document), "readme_zh_cn: README.zh-CN.md") && strings.Contains(string(document), "readme_en: README.en.md"); has != tc.readme {
				t.Fatalf("readme fields present=%v want %v\n%s", has, tc.readme, document)
			}
		})
	}
}

// An empty quantization or scenario is written as nothing: the editor used to
// turn them into "unknown" and "balanced", values the stored template never had.
func TestEditableYAMLInventsNoDefaults(t *testing.T) {
	row := editableRow("user")
	row.Specs[0].Quantization = ""
	document, err := EditableYAML(row, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	text := string(document)
	for _, invented := range []string{"unknown", "balanced", "scenario", "display_profile", "weight_bytes", "args:", "env:", "api:", "components:"} {
		if strings.Contains(text, invented) {
			t.Errorf("document states %q the template never had:\n%s", invented, text)
		}
	}
}

func TestEditableYAMLRejectsUndecodableStoredFacts(t *testing.T) {
	row := editableRow("user")
	row.Specs[1].GPUs = `{"vendor":`
	if _, err := EditableYAML(row, ""); err == nil || !strings.Contains(err.Error(), "gpus") {
		t.Fatalf("err=%v, want the corrupt gpus column named", err)
	}
}
