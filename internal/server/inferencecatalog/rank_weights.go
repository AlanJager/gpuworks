package inferencecatalog

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// validateRankScopedWeights checks a spec whose weight subfolder names the
// rank (ADR 0076). Every source has to name it: a mirror that did not would
// hand each rank the whole repository, which is exactly what the spec avoids.
// The weights of a component, the PD layouts and a spec whose primary is a
// component keep one artifact for every rank, so none of them may combine
// with it.
func validateRankScopedWeights(modelID string, spec *InferenceSpec) error {
	scoped := 0
	for _, source := range spec.Weights.Sources {
		if deploy.RankScopedSubfolder(source.Subfolder) {
			scoped++
		}
	}
	for _, component := range spec.Components {
		for _, source := range component.Weights.Sources {
			if deploy.RankScopedSubfolder(source.Subfolder) {
				return fmt.Errorf("model %s spec %s component %s: only the spec's own weights may name ${NODE_RANK} in a subfolder", modelID, spec.Name, component.Name)
			}
		}
	}
	if scoped == 0 {
		return nil
	}
	if scoped != len(spec.Weights.Sources) {
		return fmt.Errorf("model %s spec %s: either every weight source names ${NODE_RANK} in its subfolder or none does", modelID, spec.Name)
	}
	if len(spec.Components) > 0 {
		return fmt.Errorf("model %s spec %s: rank-scoped weights cannot be combined with components", modelID, spec.Name)
	}
	if spec.PDMode != "" || len(spec.PDRoles) > 0 {
		return fmt.Errorf("model %s spec %s: rank-scoped weights cannot be combined with pd_mode", modelID, spec.Name)
	}
	return nil
}
