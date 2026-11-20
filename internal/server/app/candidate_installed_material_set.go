//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"maps"
	"slices"
)

// Sources remain independently authoritative: the caller verifies each source
// manifest and its current consumer references before calling this checker.
// The derived Files set is only an installed-path proof, never a wire manifest.
func checkCandidateInstalledMaterialSets(ctx context.Context, receiver *materialReceiver, manifests map[uint64]materialManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(manifests) == 0 {
		return errors.New("installed material sources are incomplete")
	}
	consumer := materialManifest{}
	seen := make(map[materialManifestFile]bool)
	paths := make(map[string]materialManifestFile)
	for _, sourceID := range slices.Sorted(maps.Keys(manifests)) {
		for _, file := range manifests[sourceID].Files {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Identity bytes belong to the source, but their metadata must still
			// pass the original installed check before exclusion.
			if !validMaterialConsumerPath(file.Class, file.Path) || !validMaterialDigest(file.SHA256) || file.Bytes <= 0 || file.Bytes > maxMaterialReplicaBytes {
				return errors.New("installed material check received an invalid manifest entry")
			}
			if file.Class == MaterialMemberIdentity {
				continue
			}
			if prior, exists := paths[file.Path]; exists && (prior.SHA256 != file.SHA256 || prior.Bytes != file.Bytes) {
				return errors.New("source material consumer paths disagree on bytes")
			}
			if seen[file] {
				continue
			}
			// Catalog and runtime-asset entries may name the same bytes. Keep
			// their distinct classes, and also keep equal digests at other paths.
			paths[file.Path] = file
			seen[file] = true
			consumer.Files = append(consumer.Files, file)
		}
	}
	return checkInstalledMaterialManifest(ctx, receiver, consumer)
}
