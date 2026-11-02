package seed

import (
	"slices"
	"testing"
)

func TestQwen3235BA3UsesMeasuredEagerRecipe(t *testing.T) {
	_, spec := catalogSpec(t, "qwen/qwen3-235b-a22b", "int8-vllm-ascend-a3-abf48ff4")
	if !slices.Contains(spec.Args, "--enforce-eager") {
		t.Error("Qwen3-235B A3 profile lost the measured eager-mode workaround")
	}
}
