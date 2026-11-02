package seed

import "testing"

func TestTrinityMiniA3PrefersTheValidatedModelScopeSnapshot(t *testing.T) {
	_, spec := ppuSpec(t, "arcee-ai/trinity-mini", "bf16-sglang-ascend-a3-b30c5932")

	if len(spec.Weights.Sources) < 2 {
		t.Fatalf("A3 Trinity-Mini sources = %+v, want both repository providers", spec.Weights.Sources)
	}
	first := spec.Weights.Sources[0]
	if first.Provider != "modelscope" || first.Revision != "45ffa21d8c0a9ddc64b751c455d87847cc96baad" {
		t.Fatalf("A3 Trinity-Mini first source = %+v, want the validated ModelScope snapshot", first)
	}
}
