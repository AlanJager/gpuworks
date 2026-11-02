package inferencecatalog

import (
	"strings"
	"testing"
)

// Two specs of one engine that would show under the same name cannot be
// told apart on the template page, so the catalog rejects them; the same
// name under another engine, or no name at all, is fine.
func TestDisplayNamesMustBeDistinctPerModelAndEngine(t *testing.T) {
	sibling := func(c *FlatCatalog, engine, zh, en string) {
		spec := c.Models[0].Specs[0]
		spec.Name, spec.Engine, spec.DisplayNameZHCN, spec.DisplayNameEN = "sibling", engine, zh, en
		c.Models[0].Specs = append(c.Models[0].Specs, spec)
	}
	cases := []struct {
		name, engine, zh, en string
		wantErr              string
	}{
		{"same zh name", "vllm", "Ampere 及以上 · BF16", "Ampere+ · BF16 · Speculative", "share display_name_zh_cn"},
		{"same en name in other case and spacing", "vllm", "Ampere 及以上 · BF16 · 投机解码", "ampere+  ·  bf16", "share display_name_en"},
		{"same names under another engine", "sglang", "Ampere 及以上 · BF16", "Ampere+ · BF16", ""},
		{"unnamed siblings", "vllm", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := flatFixture()
			sibling(&catalog, tc.engine, tc.zh, tc.en)
			if tc.engine == "vllm" && tc.zh == "" {
				catalog.Models[0].Specs[0].DisplayNameZHCN, catalog.Models[0].Specs[0].DisplayNameEN = "", ""
			}
			err := ValidateFlatCatalog(&catalog)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// Validation trims the names and keeps them; limits count characters, not
// bytes, so a 56-character Chinese name is accepted.
func TestDisplayNamesAreTrimmedAndCountedInCharacters(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].DisplayNameZHCN = " " + strings.Repeat("卡", 56) + " "
	catalog.Models[0].Specs[0].DisplayNameEN = strings.Repeat("x", 72)
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].DisplayNameZHCN; got != strings.Repeat("卡", 56) {
		t.Fatalf("zh name = %q", got)
	}
}

// A template written before display names still imports: the retired keys
// are read, dropped from what is stored and emitted, and reported as a
// warning instead of failing the import.
func TestRetiredDisplayProfileKeysImportWithAWarning(t *testing.T) {
	raw := []byte(`models:
  - model_id: org/model
    specs:
      - name: bf16
        display_profile: general
        engine: vllm
        image: vllm/vllm-openai:latest
        weights:
          quantization: bf16
          sources:
            - huggingface: org/model
        gpus: {vendor: nvidia, cc: ">=8.0", display_target: nvidia}
`)
	catalog, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	warnings := CatalogWarnings(catalog)
	if len(warnings) != 1 || warnings[0].Code() != "CATALOG_DISPLAY_PROFILE_RETIRED" ||
		warnings[0].Params()["model_id"] != "org/model" || warnings[0].Params()["spec"] != "bf16" {
		t.Fatalf("warnings = %+v", warnings)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rows[0].Specs[0].GPUs, "display_target") {
		t.Fatalf("retired key stored: %s", rows[0].Specs[0].GPUs)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalYAML(FlatCatalog{Models: []InferenceModel{restored}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "display_profile") || strings.Contains(string(encoded), "display_target") {
		t.Fatalf("retired keys emitted again:\n%s", encoded)
	}
}

// Rows written before display names carry display_target inside the gpus
// JSON column; reading them must keep working and drop the key.
func TestStoredGPUSelectorWithRetiredDisplayTargetStillDecodes(t *testing.T) {
	rows, err := ToFlatStoreModels(flatFixture())
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Specs[0].GPUs = `{"vendor":"nvidia","cc":">=8.9","display_target":"nvidia"}`
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Specs[0].GPUs; got.Vendor != "nvidia" || got.CC != ">=8.9" || got.RetiredDisplayTarget != "" {
		t.Fatalf("gpus = %+v", got)
	}
}
