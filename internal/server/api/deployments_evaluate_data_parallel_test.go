package api

import (
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The quick form's only data-parallel input is data_parallel_size. Stated as
// 2 with every other field at its default, placement assigns the GPUs tensor
// parallelism needs and the engine would start twice that many; the preview
// answers the rule beside the data-parallel control instead of a command that
// cannot start. Degree one is the passing twin: nothing changes for it.
func TestEvaluateRefusesQuickDataParallelBeyondTheAssignedGPUs(t *testing.T) {
	st, err := store.Open("file:evaluate_data_parallel?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/data-parallel", Revision: "main", Name: "data-parallel", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	for _, tc := range []struct {
		name, engine, node, flag string
	}{
		{name: "vLLM on a chosen node", engine: "vllm", node: `"node_uuid":"field-errors-node",`, flag: "--data-parallel-size"},
		{name: "vLLM under automatic placement", engine: "vllm", flag: "--data-parallel-size"},
		{name: "SGLang on a chosen node", engine: "sglang", node: `"node_uuid":"field-errors-node",`, flag: "--dp-size"},
		{name: "SGLang under automatic placement", engine: "sglang", flag: "--dp-size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,%s"engine":%q,"quick_params":{"data_parallel_size":2}}`, model.ID, tc.node, tc.engine)
			status, raw := evaluateForFieldErrors(t, h, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			if string(raw["ok"]) != "false" {
				t.Fatalf("ok = %s, want false (body %s)", raw["ok"], raw)
			}
			errors := fieldErrorsOf(t, raw)
			if len(errors) != 1 {
				t.Fatalf("errors = %+v, want exactly the data-parallel rule", errors)
			}
			got := errors[0]
			if got.Field != "data_parallel_size" || got.Code != ErrorDataParallelGPUCount {
				t.Fatalf("field error = %+v, want data_parallel_size/%s", got, ErrorDataParallelGPUCount)
			}
			for key, want := range map[string]string{"key": "data_parallel_size", "flag": tc.flag, "dp": "2", "tp": "1", "pp": "1", "required": "2", "assigned": "1"} {
				if fmt.Sprint(got.Params[key]) != want {
					t.Fatalf("params[%q] = %v, want %s (all: %+v)", key, got.Params[key], want, got.Params)
				}
			}

			twin := fmt.Sprintf(`{"model_id":%d,%s"engine":%q,"quick_params":{"data_parallel_size":1}}`, model.ID, tc.node, tc.engine)
			status, raw = evaluateForFieldErrors(t, h, twin)
			if status != http.StatusOK || string(raw["ok"]) != "true" {
				t.Fatalf("degree one: status = %d body = %s", status, raw)
			}
			if _, present := raw["errors"]; present {
				t.Fatalf("degree one produced errors: %s", raw["errors"])
			}
		})
	}
}
