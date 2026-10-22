package inferencecatalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// schemaProperties flattens a JSON-Schema fragment's properties/items chain
// into the set of property names at each object level this test cares about.
func schemaProperties(node map[string]any) map[string]any {
	properties, _ := node["properties"].(map[string]any)
	return properties
}

func items(node map[string]any) map[string]any {
	raw, _ := node["items"].(map[string]any)
	return raw
}

// yamlKeys reflects a struct's `yaml:"..."` field names (without omitempty).
func yamlKeys(value any) []string {
	var keys []string
	typ := reflect.TypeOf(value)
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		keys = append(keys, strings.Split(tag, ",")[0])
	}
	return keys
}

// TestJSONSchemaCoversStructFields keeps the hand-maintained editor schema
// from drifting away from the decoding structs: every YAML key the strict
// decoder accepts must appear in the schema, and vice versa.
func TestJSONSchemaCoversStructFields(t *testing.T) {
	schema := JSONSchema()
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("schema draft marker missing: %v", schema["$schema"])
	}
	// The schema must serialize cleanly (the API serves it as JSON).
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("schema is not JSON-serializable: %v", err)
	}
	modelProps := schemaProperties(items(schemaProperties(schema)["models"].(map[string]any)))
	specProps := schemaProperties(items(modelProps["specs"].(map[string]any)))

	check := func(where string, properties map[string]any, structure any) {
		structureKeys := map[string]bool{}
		for _, key := range yamlKeys(structure) {
			structureKeys[key] = true
		}
		schemaKeys := map[string]bool{}
		for key := range properties {
			schemaKeys[key] = true
		}
		for key := range structureKeys {
			if !schemaKeys[key] {
				t.Errorf("%s: struct key %q missing from editor schema", where, key)
			}
		}
		for key := range schemaKeys {
			if !structureKeys[key] {
				t.Errorf("%s: schema key %q not accepted by the strict decoder", where, key)
			}
		}
	}
	check("model", modelProps, InferenceModel{})
	check("spec", specProps, InferenceSpec{})
	check("gpus", schemaProperties(specProps["gpus"].(map[string]any)), GPUSelector{})
	apiProps := schemaProperties(specProps["api"].(map[string]any))
	check("api", apiProps, inferenceapi.Contract{})
	metricsProps := schemaProperties(apiProps["metrics"].(map[string]any))
	check("api.metrics", metricsProps, inferenceapi.Metrics{})

	// Metric targets and inheritable engines come from the vocabulary, so a
	// metric added there is offered by the editor without a schema edit.
	targets := metricsProps["map"].(map[string]any)["additionalProperties"].(map[string]any)["oneOf"].([]any)
	nameEnum := targets[0].(map[string]any)["enum"].([]any)
	if len(nameEnum) != len(inferencemetrics.Names()) {
		t.Fatalf("api.metrics target enum has %d names, vocabulary %d", len(nameEnum), len(inferencemetrics.Names()))
	}
	objectProps := schemaProperties(targets[1].(map[string]any))
	if len(objectProps) != 2 || objectProps["to"] == nil || objectProps["scale"] == nil {
		t.Fatalf("api.metrics long-form target = %v, want {to, scale}", objectProps)
	}
	inherit := metricsProps["inherit"].(map[string]any)["enum"].([]any)
	for _, engine := range inherit {
		if !inferencemetrics.HasEngineRules(engine.(string)) {
			t.Fatalf("api.metrics inherit offers %v, which has no built-in mapping", engine)
		}
	}

	// Operation/engine vocabularies must come from the code-owned registries.
	operationEnum, _ := specProps["api"].(map[string]any)
	if operationEnum == nil {
		t.Fatal("spec.api missing from schema")
	}
	operations := schemaProperties(operationEnum)["operations"].(map[string]any)
	declared := items(operations)["properties"].(map[string]any)["type"].(map[string]any)["enum"].([]any)
	if len(declared) < 8 {
		t.Fatalf("operation enum suspiciously small: %v", declared)
	}
	for _, value := range declared {
		if value == "openai.chat" {
			return
		}
	}
	t.Fatalf("operation enum is missing openai.chat: %v", declared)
}
