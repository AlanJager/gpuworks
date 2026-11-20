package inferencecatalog

import (
	"strings"
	"testing"
)

// A spec's own weights may name the rank in their subfolder (ADR 0076); the
// YAML form round-trips, and the shapes that would hand a rank anything but
// its own part are refused at import.
func TestRankScopedWeightSources(t *testing.T) {
	raw := []byte(`models:
  - model_id: Org/Ranked
    specs:
      - name: tp2
        engine: vllm
        image: vllm/vllm-openai:latest
        weight_bytes: 1000
        weights:
          quantization: bf16
          sources:
            - modelscope:
                repo_id: Org/Ranked-tp2
                subfolder: rank${NODE_RANK}
            - huggingface:
                repo_id: Org/Ranked-tp2
                subfolder: rank${GPUWORKS_NODE_RANK}
        gpus:
          vendor: nvidia
`)
	catalog, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].Weights.Sources[0].Subfolder; got != "rank${NODE_RANK}" {
		t.Fatalf("subfolder = %q", got)
	}
	encoded, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "subfolder: rank${NODE_RANK}") {
		t.Fatalf("marshaled YAML lost the rank subfolder:\n%s", encoded)
	}

	for _, tc := range []struct {
		name string
		edit func(*InferenceSpec)
		want string
	}{
		{name: "another variable", edit: func(spec *InferenceSpec) {
			spec.Weights.Sources[0].Subfolder = "tp${TP_SIZE}"
		}, want: "may name only ${NODE_RANK}, not ${TP_SIZE}"},
		{name: "a mirror without the rank", edit: func(spec *InferenceSpec) {
			spec.Weights.Sources[1].Subfolder = ""
		}, want: "either every weight source names ${NODE_RANK}"},
		{name: "with components", edit: func(spec *InferenceSpec) {
			spec.Components = []Component{{Name: "main", Role: "primary", Weights: spec.Weights, WeightBytes: 1000, MountPath: "/model"}}
		}, want: "only the spec's own weights may name ${NODE_RANK}"},
		{name: "with pd_mode", edit: func(spec *InferenceSpec) {
			spec.PDMode = "nixl"
		}, want: ""},
		{name: "unsafe expansion", edit: func(spec *InferenceSpec) {
			spec.Weights.Sources[0].Subfolder = "../rank${NODE_RANK}"
		}, want: "unsafe model repository subfolder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := DecodeYAML(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&catalog.Models[0].Specs[0])
			err = ValidateFlatCatalog(&catalog)
			if err == nil {
				t.Fatal("accepted")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The rule applied to a spec value alone: plain subfolders are untouched,
// a PD spec with a rank subfolder is refused by the rank rule itself.
func TestValidateRankScopedWeightsRefusesPD(t *testing.T) {
	spec := &InferenceSpec{Name: "pd", PDMode: "nixl", Weights: Weights{Sources: []WeightSource{{Provider: "modelscope", RepoID: "o/r", Subfolder: "rank${NODE_RANK}"}}}}
	if err := validateRankScopedWeights("o/m", spec); err == nil || !strings.Contains(err.Error(), "pd_mode") {
		t.Fatalf("pd spec = %v", err)
	}
	spec.PDMode, spec.Weights.Sources[0].Subfolder = "nixl", "rank0"
	if err := validateRankScopedWeights("o/m", spec); err != nil {
		t.Fatalf("plain subfolder refused: %v", err)
	}
}
