package inferencecatalog

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// validateManagedDirs normalizes and checks a spec's writable directories
// (InferenceSpec.Dirs, ADR 0077). The declaration's own rules are
// deploy.NormalizeManagedDirs; on top of them a directory may not overlap
// another container path the spec owns: the primary model mount, a component's
// mount_path or an asset's unpack_to. It runs after the components are
// normalized, because the primary mount is the primary component's.
func validateManagedDirs(where string, spec *InferenceSpec) error {
	if len(spec.Dirs) == 0 {
		spec.Dirs = nil
		return nil
	}
	if err := deploy.NormalizeManagedDirs(spec.Dirs); err != nil {
		return fmt.Errorf("%s %w", where, err)
	}
	owned := []struct{ what, path string }{{"the model mount", deploy.DefaultModelMount}}
	for _, component := range spec.Components {
		if component.Role == "primary" {
			owned[0].path = component.MountPath
			continue
		}
		owned = append(owned, struct{ what, path string }{"component " + component.Name + " mount_path", component.MountPath})
	}
	for _, asset := range spec.Assets {
		owned = append(owned, struct{ what, path string }{"asset " + asset.File + " unpack_to", asset.UnpackTo})
	}
	for _, dir := range spec.Dirs {
		for _, other := range owned {
			if directoryWithin(dir.MountPath, other.path) || directoryWithin(other.path, dir.MountPath) {
				return fmt.Errorf("%s dir %s mount_path %q overlaps %s %q", where, dir.Name, dir.MountPath, other.what, other.path)
			}
		}
	}
	return nil
}
