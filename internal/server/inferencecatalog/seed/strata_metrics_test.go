package seed

import (
	"encoding/json"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// Every path the Strata template maps is a number (a string, for a
// comparison) in a real Strata 0.1.38 /metrics document taken mid-request, so
// a renamed field fails here rather than as an empty panel; and the
// declaration resolves to a JSON observation.
func TestStrataMetricsMapRealDocumentNumbers(t *testing.T) {
	raw, err := os.ReadFile("../../../inferencemetrics/testdata/strata-gw6-metrics.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	_, spec := ppuSpec(t, "ista-daslab/qwen3.8-flash-next-strata", "iq2xs-strata-nvidia-single-gpu")
	if spec.API == nil || spec.API.Metrics == nil || spec.API.Metrics.Format != "json" || len(spec.API.Metrics.Map) == 0 {
		t.Fatalf("strata metrics = %+v", spec.API)
	}
	for source := range spec.API.Metrics.Map {
		parsed, ok := inferencemetrics.ParseJSONSource(source)
		if !ok {
			t.Fatalf("%s is not a JSON path", source)
		}
		current := document
		for _, key := range parsed.Keys {
			object, isObject := current.(map[string]any)
			if !isObject {
				t.Fatalf("%s: %s is not inside an object", source, key)
			}
			current = object[key]
		}
		if parsed.Compare != "" {
			if _, isString := current.(string); !isString {
				t.Errorf("%s compares %T in the document, not a string", source, current)
			}
		} else if _, isNumber := current.(float64); !isNumber {
			t.Errorf("%s is %T in the document, not a number", source, current)
		}
	}
	observation := deploy.ResolveObservation(spec.Engine, spec.API.Metrics)
	if observation.Instance.MetricFormat != "json" || len(observation.Instance.MetricRules) != len(spec.API.Metrics.Map) {
		t.Fatalf("resolved = %+v", observation.Instance)
	}
}
