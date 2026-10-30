package planning

import (
	"gpuworks.ai/gpuworks/internal/server/store"
	"testing"
)

func TestStoredDeploymentPoolNeedIncludesDataParallel(t *testing.T) {
	dep := &store.Deployment{Engine: "vllm", QuickParams: `{"data_parallel_size":2}`, Placement: `{"tp":1}`}
	if got := ReplicaGPUNeed(dep); got != 2 {
		t.Fatalf("pool need = %d, want 2", got)
	}
	dep.ExtraArgs = "--data-parallel-size 4"
	if got := ReplicaGPUNeed(dep); got != 4 {
		t.Fatalf("explicit DP need = %d, want 4", got)
	}
}
