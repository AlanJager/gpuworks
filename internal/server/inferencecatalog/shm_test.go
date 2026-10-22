package inferencecatalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

const shmSpecYAML = `models:
- model_id: Org/Model
  specs:
  - name: deepinfer
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    shm_bytes: 429496729600
`

// shm_bytes is read from the catalog document, survives the store projection
// and comes back out of it, so the editor's YAML round-trips it.
func TestSpecShmBytesParsesAndRoundTripsThroughTheStoreRows(t *testing.T) {
	catalog, err := ParseYAML([]byte(shmSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].ShmBytes; got != 429_496_729_600 {
		t.Fatalf("shm_bytes = %d", got)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Specs[0].ShmBytes != 429_496_729_600 {
		t.Fatalf("stored shm_bytes = %d", rows[0].Specs[0].ShmBytes)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil || restored.Specs[0].ShmBytes != 429_496_729_600 {
		t.Fatalf("restored = %+v, %v", restored.Specs, err)
	}

	// A spec that states none stores zero and writes no key back out.
	plain := flatFixture()
	rows, err = ToFlatStoreModels(plain)
	if err != nil || rows[0].Specs[0].ShmBytes != 0 {
		t.Fatalf("plain rows = %+v, %v", rows[0].Specs, err)
	}
}

func TestSpecShmBytesValidation(t *testing.T) {
	for name, test := range map[string]struct {
		value   int64
		invalid bool
	}{
		"absent reads as zero":  {value: 0},
		"one byte":              {value: 1},
		"400 GiB":               {value: 400 << 30},
		"exactly 16 TiB":        {value: MaxSpecShmBytes},
		"one byte over 16 TiB":  {value: MaxSpecShmBytes + 1, invalid: true},
		"negative":              {value: -1, invalid: true},
		"far beyond the limit":  {value: 1 << 62, invalid: true},
		"the largest int64":     {value: 1<<63 - 1, invalid: true},
		"most negative integer": {value: -1 << 63, invalid: true},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := flatFixture()
			catalog.Models[0].Specs[0].ShmBytes = test.value
			err := ValidateFlatCatalog(&catalog)
			if !test.invalid {
				if err != nil {
					t.Fatalf("shm_bytes %d rejected: %v", test.value, err)
				}
				return
			}
			var productErr *messages.ProductError
			if !errors.As(err, &productErr) {
				t.Fatalf("shm_bytes %d: error = %v, want a product error", test.value, err)
			}
			if productErr.Code() != messages.ErrorCatalogShmBytesInvalid || productErr.Code() != "CATALOG_SHM_BYTES_INVALID" {
				t.Fatalf("code = %q", productErr.Code())
			}
			params := productErr.Params()
			if params["model"] != "org/model" || params["spec"] != "bf16-balanced" || params["shm_bytes"] != test.value || params["max_bytes"] != MaxSpecShmBytes {
				t.Fatalf("params = %#v", params)
			}
			if !strings.Contains(productErr.Error(), "shm_bytes") {
				t.Fatalf("fallback = %q", productErr.Error())
			}
		})
	}
}

func TestSpecShmBytesIsInTheEditorSchemaAndTheStrictDecoder(t *testing.T) {
	schema := JSONSchema()
	modelProps := schemaProperties(items(schemaProperties(schema)["models"].(map[string]any)))
	specProps := schemaProperties(items(modelProps["specs"].(map[string]any)))
	property, ok := specProps["shm_bytes"].(map[string]any)
	if !ok {
		t.Fatal("editor schema does not describe shm_bytes")
	}
	want := map[string]any{"type": "integer", "minimum": 1, "maximum": MaxSpecShmBytes}
	for key, value := range want {
		if !reflect.DeepEqual(property[key], value) {
			t.Errorf("schema shm_bytes %s = %#v, want %#v", key, property[key], value)
		}
	}
	if description, _ := property["description"].(string); !strings.Contains(description, "/dev/shm") {
		t.Errorf("schema shm_bytes description = %q", description)
	}

	// The strict decoder still refuses a misspelling of the key.
	misspelled := strings.Replace(shmSpecYAML, "shm_bytes", "shm_byte", 1)
	if _, err := ParseYAML([]byte(misspelled)); err == nil {
		t.Fatal("a misspelled shm_bytes key was accepted")
	}
}
