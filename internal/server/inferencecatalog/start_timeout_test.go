package inferencecatalog

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const startTimeoutSpecYAML = `models:
- model_id: Org/Model
  specs:
  - name: long-first-start
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    start_timeout_sec: 5400
`

// start_timeout_sec is read from the catalog document, survives the store
// projection and comes back out of it, so the editor's YAML round-trips it.
func TestSpecStartTimeoutParsesAndRoundTripsThroughTheStoreRows(t *testing.T) {
	catalog, err := ParseYAML([]byte(startTimeoutSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].StartTimeoutSec; got != 5400 {
		t.Fatalf("start_timeout_sec = %d", got)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].Specs[0].StartTimeoutSec; got != 5400 {
		t.Fatalf("stored start_timeout_sec = %d", got)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil || restored.Specs[0].StartTimeoutSec != 5400 {
		t.Fatalf("restored = %+v, %v", restored.Specs, err)
	}
	out, err := MarshalYAML(FlatCatalog{Models: []InferenceModel{restored}})
	if err != nil || !strings.Contains(string(out), "start_timeout_sec: 5400") {
		t.Fatalf("rendered YAML lacks start_timeout_sec (err=%v):\n%s", err, out)
	}

	// A spec that states none stores zero and writes no key back out.
	rows, err = ToFlatStoreModels(flatFixture())
	if err != nil || rows[0].Specs[0].StartTimeoutSec != 0 {
		t.Fatalf("plain rows = %+v, %v", rows[0].Specs, err)
	}
	plain, err := FromFlatStoreModel(rows[0])
	if err != nil || plain.Specs[0].StartTimeoutSec != 0 {
		t.Fatalf("plain restored = %+v, %v", plain.Specs, err)
	}
	out, err = MarshalYAML(FlatCatalog{Models: []InferenceModel{plain}})
	if err != nil || strings.Contains(string(out), "start_timeout_sec") {
		t.Fatalf("plain YAML states start_timeout_sec (err=%v):\n%s", err, out)
	}
}

// The spec's bounds are the deployment's: a value no deployment could state
// is refused with a stable code, and 0 reads as unset.
func TestSpecStartTimeoutBounds(t *testing.T) {
	for _, value := range []string{"0", "60", "86400"} {
		yaml := strings.Replace(startTimeoutSpecYAML, "start_timeout_sec: 5400", "start_timeout_sec: "+value, 1)
		if _, err := ParseYAML([]byte(yaml)); err != nil {
			t.Fatalf("start_timeout_sec: %s rejected: %v", value, err)
		}
	}
	for _, value := range []string{"-1", "59", "86401"} {
		yaml := strings.Replace(startTimeoutSpecYAML, "start_timeout_sec: 5400", "start_timeout_sec: "+value, 1)
		_, err := ParseYAML([]byte(yaml))
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != messages.ErrorCatalogStartTimeoutInvalid {
			t.Fatalf("start_timeout_sec: %s err = %v, want %s", value, err, messages.ErrorCatalogStartTimeoutInvalid)
		}
		if product.Params()["min_sec"] != store.DeploymentStartTimeoutSecLo || product.Params()["max_sec"] != store.DeploymentStartTimeoutSecHi || product.Params()["spec"] != "long-first-start" {
			t.Fatalf("params = %v", product.Params())
		}
	}
	for _, value := range []string{`"5400"`, "1.5", "[60]"} {
		yaml := strings.Replace(startTimeoutSpecYAML, "start_timeout_sec: 5400", "start_timeout_sec: "+value, 1)
		if _, err := ParseYAML([]byte(yaml)); err == nil {
			t.Errorf("start_timeout_sec: %s was accepted", value)
		}
	}
}

func TestSpecStartTimeoutIsInTheEditorSchema(t *testing.T) {
	schema := JSONSchema()
	modelProps := schemaProperties(items(schemaProperties(schema)["models"].(map[string]any)))
	specProps := schemaProperties(items(modelProps["specs"].(map[string]any)))
	property, ok := specProps["start_timeout_sec"].(map[string]any)
	if !ok {
		t.Fatal("editor schema does not describe start_timeout_sec")
	}
	if property["type"] != "integer" || property["minimum"] != store.DeploymentStartTimeoutSecLo || property["maximum"] != store.DeploymentStartTimeoutSecHi {
		t.Errorf("schema start_timeout_sec = %#v", property)
	}
}
