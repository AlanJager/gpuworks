package seed

import (
	"slices"
	"testing"
)

func TestQwen314BGGUFA3SelectsOneCheckpointAndRunsEager(t *testing.T) {
	_, spec := ppuSpec(t, "qwen/qwen3-14b-gguf", "fp16-sglang-ascend-a3-41226ec8")

	if !slices.Contains(spec.Args, "--disable-cuda-graph") {
		t.Error("A3 GGUF profile must run without graph capture")
	}
	for index := 0; index+1 < len(spec.Args); index++ {
		if spec.Args[index] != "--model-path" {
			continue
		}
		if got := spec.Args[index+1]; got != "/model/Qwen3-14B-Q4_K_M.gguf" {
			t.Fatalf("A3 GGUF profile selected %q, want the validated Q4_K_M checkpoint", got)
		}
		return
	}
	t.Fatal("A3 GGUF profile did not select one checkpoint from the multi-file repository")
}
