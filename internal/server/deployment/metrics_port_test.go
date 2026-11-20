package deployment

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The model's own operations replace the template's in the snapshot, but the
// template's metrics port is a launch fact (the instance reserves it and the
// launch states it): it stays, and the rest of api.metrics does not, because
// the mapping is read from the tier's current declaration.
func TestCurrentModelAPIContractKeepsTheTemplatesMetricsPort(t *testing.T) {
	declared := `{"operations":[{"type":"openai.chat"}],"metrics":{"endpoint":"/m","port":"separate","map":{"x":"requests_running"}}}`
	selection := &templateSelection{APIContract: json.RawMessage(declared)}
	applyCurrentModelAPIContract(selection, &store.Model{Operations: `[{"type":"openai.embeddings"}]`})
	var got inferenceapi.Contract
	if err := json.Unmarshal(selection.APIContract, &got); err != nil {
		t.Fatal(err)
	}
	if got.Operations[0].Type != inferenceapi.OperationEmbeddings {
		t.Fatalf("operations=%+v", got.Operations)
	}
	if got.Metrics == nil || got.Metrics.Port != "separate" || got.Metrics.Endpoint != "" || len(got.Metrics.Map) != 0 {
		t.Fatalf("metrics=%+v, want only the port", got.Metrics)
	}
	if !inferenceapi.SeparateMetricsPort(selection.APIContract) {
		t.Fatalf("the snapshot lost the metrics port: %s", selection.APIContract)
	}

	// No port declared: the snapshot gains no metrics at all.
	selection = &templateSelection{APIContract: json.RawMessage(`{"operations":[{"type":"openai.chat"}],"metrics":{"endpoint":"/m"}}`)}
	applyCurrentModelAPIContract(selection, &store.Model{Operations: `[{"type":"openai.embeddings"}]`})
	got = inferenceapi.Contract{}
	if err := json.Unmarshal(selection.APIContract, &got); err != nil || got.Metrics != nil {
		t.Fatalf("metrics=%+v err=%v, want none", got.Metrics, err)
	}
}
