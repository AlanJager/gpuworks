package inferencecatalog

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// containerPathClaim is one container path a spec puts something at, and the
// field that says so (in the words an import error names it by).
type containerPathClaim struct {
	field string
	path  string
	asset bool
}

// specContainerPaths lists every container path the spec claims, after the
// one the platform keeps for its own files (proto.PlatformFileDir: the peers
// file, the pre-start wrapper), in the order the spec's fields are
// documented: the primary model mount, each other component's mount_path,
// each asset's unpack_to, each dir's mount_path. It runs after the components
// are normalized, because the primary mount is the primary component's.
func specContainerPaths(spec *InferenceSpec) []containerPathClaim {
	claims := []containerPathClaim{
		{field: "the platform directory", path: proto.PlatformFileDir},
		{field: "the model mount", path: deploy.DefaultModelMount},
	}
	for _, component := range spec.Components {
		if component.Role == "primary" {
			claims[1].path = component.MountPath
			continue
		}
		claims = append(claims, containerPathClaim{field: "component " + component.Name + " mount_path", path: component.MountPath})
	}
	for _, asset := range spec.Assets {
		claims = append(claims, containerPathClaim{field: "asset " + asset.File + " unpack_to", path: asset.UnpackTo, asset: true})
	}
	for _, dir := range spec.Dirs {
		claims = append(claims, containerPathClaim{field: "dir " + dir.Name + " mount_path", path: dir.MountPath})
	}
	return claims
}

// validateContainerPaths is the import half of the one container path rule
// (proto.ContainerPathsOverlap; the launch half is deploy's
// validateResolvedMounts): no two paths the spec claims may be the same or
// nest, whatever kind of mount each is. Assets are the one exception among
// themselves: several archives may unpack into one directory. It is refused
// with CATALOG_MOUNT_PATH_CONFLICT.
func validateContainerPaths(modelID string, spec *InferenceSpec) error {
	claims := specContainerPaths(spec)
	for index, claim := range claims {
		for _, earlier := range claims[:index] {
			if claim.asset && earlier.asset {
				continue
			}
			if !proto.ContainerPathsOverlap(claim.path, earlier.path) {
				continue
			}
			return messages.NewProductError(messages.ErrorCatalogMountPathConflict,
				fmt.Sprintf("model %s spec %s %s %q overlaps %s %q", modelID, spec.Name, claim.field, claim.path, earlier.field, earlier.path),
				map[string]any{"model": modelID, "spec": spec.Name, "field": claim.field, "path": claim.path, "other_field": earlier.field, "other_path": earlier.path})
		}
	}
	return nil
}
