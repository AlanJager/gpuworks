package api

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func instanceGPUCount(inst store.Instance) int {
	var indexes []int
	if json.Unmarshal([]byte(inst.GPUIndexes), &indexes) == nil && len(indexes) > 0 {
		return len(indexes)
	}
	var spec proto.InstanceDesiredState
	if json.Unmarshal([]byte(inst.DesiredStateJSON), &spec) == nil {
		return len(spec.GPUIndexes)
	}
	return 0
}
