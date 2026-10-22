package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// memlock_unlimited is stored on the spec row, read back unchanged, and kept
// when an import of a user spec rewrites the model's other specs.
func TestInferenceSpecMemlockUnlimitedRoundTrip(t *testing.T) {
	st, err := Open("file:inference_spec_memlock_round_trip?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	spec := func(name string, memlock bool) InferenceSpecRow {
		return InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest",
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Memlock"}]`, GPUs: `{"vendor":"nvidia"}`, MemlockUnlimited: memlock,
		}
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{{
		ModelID: "acme/memlock", DisplayTitle: "Memlock", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []InferenceSpecRow{spec("pinned", true), spec("plain", false)},
	}}); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]bool {
		model, err := st.InferenceModelByModelID("acme/memlock")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, row := range model.Specs {
			got[row.Name] = row.MemlockUnlimited
		}
		return got
	}
	if got := read(); len(got) != 2 || !got["pinned"] || got["plain"] {
		t.Fatalf("memlock_unlimited = %v", got)
	}
	user := spec("mine", true)
	if err := st.ImportUserInferenceCatalog([]InferenceModel{{ModelID: "acme/memlock", Specs: []InferenceSpecRow{user}}}, false); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 3 || !got["pinned"] || got["plain"] || !got["mine"] {
		t.Fatalf("memlock_unlimited after import = %v", got)
	}
}
