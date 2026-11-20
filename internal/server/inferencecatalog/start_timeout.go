package inferencecatalog

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// validateStartTimeout checks a spec's start_timeout_sec against the same
// bounds a deployment's own start_timeout_sec has (store.ValidStartTimeoutSec),
// so whatever a template states is a value a deployment could also have
// stated. Zero means the spec states none (an absent key and an explicit 0
// read the same); anything else outside the bounds is refused with the stable
// code CATALOG_START_TIMEOUT_INVALID.
func validateStartTimeout(modelID string, spec *InferenceSpec) error {
	if spec.StartTimeoutSec == 0 || store.ValidStartTimeoutSec(spec.StartTimeoutSec) {
		return nil
	}
	return messages.NewProductError(
		messages.ErrorCatalogStartTimeoutInvalid,
		fmt.Sprintf("model %s spec %s start_timeout_sec %d must be between %d and %d seconds", modelID, spec.Name, spec.StartTimeoutSec, store.DeploymentStartTimeoutSecLo, store.DeploymentStartTimeoutSecHi),
		map[string]any{"model": modelID, "spec": spec.Name, "start_timeout_sec": spec.StartTimeoutSec, "min_sec": store.DeploymentStartTimeoutSecLo, "max_sec": store.DeploymentStartTimeoutSecHi},
	)
}
