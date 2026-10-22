package inferencecatalog

import (
	"reflect"
	"strings"
	"testing"
)

const memlockSpecYAML = `models:
- model_id: Org/Model
  specs:
  - name: deepinfer
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    memlock_unlimited: true
`

// memlock_unlimited is read from the catalog document, survives the store
// projection and comes back out of it, so the editor's YAML round-trips it.
func TestSpecMemlockUnlimitedParsesAndRoundTripsThroughTheStoreRows(t *testing.T) {
	catalog, err := ParseYAML([]byte(memlockSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.Models[0].Specs[0].MemlockUnlimited {
		t.Fatal("memlock_unlimited was not read")
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Specs[0].MemlockUnlimited {
		t.Fatal("stored memlock_unlimited = false")
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil || !restored.Specs[0].MemlockUnlimited {
		t.Fatalf("restored = %+v, %v", restored.Specs, err)
	}

	// A spec that states none stores false and writes no key back out.
	rows, err = ToFlatStoreModels(flatFixture())
	if err != nil || rows[0].Specs[0].MemlockUnlimited {
		t.Fatalf("plain rows = %+v, %v", rows[0].Specs, err)
	}
	plain, err := FromFlatStoreModel(rows[0])
	if err != nil || plain.Specs[0].MemlockUnlimited {
		t.Fatalf("plain restored = %+v, %v", plain.Specs, err)
	}
}

func TestSpecMemlockUnlimitedMustBeABoolean(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		yaml := strings.Replace(memlockSpecYAML, "memlock_unlimited: true", "memlock_unlimited: "+value, 1)
		catalog, err := ParseYAML([]byte(yaml))
		if err != nil {
			t.Fatalf("memlock_unlimited: %s rejected: %v", value, err)
		}
		if got := catalog.Models[0].Specs[0].MemlockUnlimited; got != (value == "true") {
			t.Fatalf("memlock_unlimited: %s read as %v", value, got)
		}
	}
	for _, value := range []string{"1", "-1", `"unlimited"`, "[true]", "{a: b}"} {
		yaml := strings.Replace(memlockSpecYAML, "memlock_unlimited: true", "memlock_unlimited: "+value, 1)
		if _, err := ParseYAML([]byte(yaml)); err == nil {
			t.Errorf("memlock_unlimited: %s was accepted", value)
		}
	}
	// The strict decoder still refuses a misspelling of the key.
	misspelled := strings.Replace(memlockSpecYAML, "memlock_unlimited", "memlock_unlimit", 1)
	if _, err := ParseYAML([]byte(misspelled)); err == nil {
		t.Fatal("a misspelled memlock_unlimited key was accepted")
	}
}

func TestSpecMemlockUnlimitedIsInTheEditorSchema(t *testing.T) {
	schema := JSONSchema()
	modelProps := schemaProperties(items(schemaProperties(schema)["models"].(map[string]any)))
	specProps := schemaProperties(items(modelProps["specs"].(map[string]any)))
	property, ok := specProps["memlock_unlimited"].(map[string]any)
	if !ok {
		t.Fatal("editor schema does not describe memlock_unlimited")
	}
	if !reflect.DeepEqual(property["type"], "boolean") {
		t.Errorf("schema memlock_unlimited type = %#v", property["type"])
	}
	if description, _ := property["description"].(string); !strings.Contains(description, "memlock") {
		t.Errorf("schema memlock_unlimited description = %q", description)
	}
}
