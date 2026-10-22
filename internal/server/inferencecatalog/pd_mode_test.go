package inferencecatalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// A PD-only spec names a mode from the PD mode catalog, for the engine that
// mode runs on, and keeps it through the store projection (plan D15).
func TestFlatCatalogPDModeSpec(t *testing.T) {
	catalog := flatFixture()
	pd := catalog.Models[0].Specs[0]
	pd.Name, pd.PDMode, pd.Env = "bf16-vllm-nvidia-pd", " vllm-nixl ", map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "DS"}
	catalog.Models[0].Specs = append(catalog.Models[0].Specs, pd)
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Models[0].Specs[1].PDMode != "vllm-nixl" {
		t.Fatalf("pd_mode = %q", catalog.Models[0].Specs[1].PDMode)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Specs[0].PDMode != "" || rows[0].Specs[1].PDMode != "vllm-nixl" {
		t.Fatalf("rows = %q %q", rows[0].Specs[0].PDMode, rows[0].Specs[1].PDMode)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil || restored.Specs[1].PDMode != "vllm-nixl" {
		t.Fatalf("restored = %+v %v", restored.Specs, err)
	}

	for value, want := range map[string]string{"vllm-magic": "unknown pd_mode", "sglang-mooncake": "unknown pd_mode"} {
		broken := flatFixture()
		broken.Models[0].Specs[0].PDMode = value
		if err := ValidateFlatCatalog(&broken); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", value, err)
		}
	}
	wrongEngine := flatFixture()
	wrongEngine.Models[0].Specs[0].Engine, wrongEngine.Models[0].Specs[0].PDMode = "sglang", "vllm-nixl"
	if err := ValidateFlatCatalog(&wrongEngine); err == nil || !strings.Contains(err.Error(), "runs on vllm") {
		t.Fatalf("engine mismatch: %v", err)
	}
}

func TestFlatCatalogYAMLReadsPDMode(t *testing.T) {
	doc := `models:
- model_id: Org/Model
  specs:
  - name: bf16-vllm-nvidia-pd
    pd_mode: vllm-nixl
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
`
	catalog, err := ParseYAML([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Models[0].Specs[0].PDMode != "vllm-nixl" {
		t.Fatalf("spec = %+v", catalog.Models[0].Specs[0])
	}
	if schema := string(mustJSONSchema(t)); !strings.Contains(schema, `"pd_mode"`) || !strings.Contains(schema, `"vllm-nixl"`) {
		t.Fatal("editor schema does not describe pd_mode")
	}
}

func mustJSONSchema(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(JSONSchema())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
