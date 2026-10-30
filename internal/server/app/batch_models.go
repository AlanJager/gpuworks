package app

import (
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// batchModels tells the batch service which served names the platform has: a
// name is available when an active deployment serves it, and held when every
// such deployment sleeps by an operator's or a schedule's order (which a
// request does not wake; Deployment.SleepHeld is cleared by any wake).
type batchModels struct {
	st *store.Store
}

func (m batchModels) Lookup(name string) (batch.ModelState, error) {
	rows, err := m.st.DeploymentsByServedName(name)
	if err != nil {
		return batch.ModelUnknown, err
	}
	if len(rows) == 0 {
		return batch.ModelUnknown, nil
	}
	for _, d := range rows {
		if !d.SleepHeld {
			return batch.ModelAvailable, nil
		}
	}
	return batch.ModelSleepHeld, nil
}
