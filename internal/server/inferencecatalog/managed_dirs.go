package inferencecatalog

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// validateManagedDirs normalizes and checks a spec's writable directories
// (InferenceSpec.Dirs, ADR 0077) by the declaration's own rules
// (deploy.NormalizeManagedDirs). Where they may be mounted is the spec's one
// container path check (validateContainerPaths), which runs after it.
func validateManagedDirs(where string, spec *InferenceSpec) error {
	if len(spec.Dirs) == 0 {
		spec.Dirs = nil
		return nil
	}
	if err := deploy.NormalizeManagedDirs(spec.Dirs); err != nil {
		return fmt.Errorf("%s %w", where, err)
	}
	return nil
}
