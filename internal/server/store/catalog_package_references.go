package store

import (
	"encoding/json"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ReferencedCatalogPackageDigests returns every catalog-package identity that
// must survive garbage collection. Active catalog rows are not enough: an
// existing deployment keeps its immutable template snapshot executable after
// the source template is edited, and an instance desired state is likewise a
// durable launch record.
func (s *Store) ReferencedCatalogPackageDigests() ([]string, error) {
	references := map[string]struct{}{}
	var modelDigests []string
	if err := s.db.Model(&InferenceModel{}).Where("package_digest <> ''").Pluck("package_digest", &modelDigests).Error; err != nil {
		return nil, err
	}
	for _, digest := range modelDigests {
		addCatalogPackageDigest(references, digest)
	}
	var specDigests []string
	if err := s.db.Model(&InferenceSpecRow{}).Where("package_digest <> ''").Pluck("package_digest", &specDigests).Error; err != nil {
		return nil, err
	}
	for _, digest := range specDigests {
		addCatalogPackageDigest(references, digest)
	}

	var snapshots []string
	if err := s.db.Model(&Deployment{}).Where("template_snapshot <> ''").Pluck("template_snapshot", &snapshots).Error; err != nil {
		return nil, err
	}
	for _, raw := range snapshots {
		var snapshot struct {
			RuntimeAssets []proto.RuntimeAsset `json:"runtime_assets"`
		}
		if json.Unmarshal([]byte(raw), &snapshot) != nil {
			continue
		}
		addRuntimeAssetPackageDigests(references, snapshot.RuntimeAssets)
	}

	var desiredStates []string
	if err := s.db.Model(&Instance{}).Where("spec <> ''").Pluck("spec", &desiredStates).Error; err != nil {
		return nil, err
	}
	for _, raw := range desiredStates {
		var desired proto.InstanceDesiredState
		if json.Unmarshal([]byte(raw), &desired) != nil {
			continue
		}
		addRuntimeAssetPackageDigests(references, desired.RuntimeAssets)
	}

	result := make([]string, 0, len(references))
	for digest := range references {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result, nil
}

func addRuntimeAssetPackageDigests(target map[string]struct{}, assets []proto.RuntimeAsset) {
	for _, asset := range assets {
		addCatalogPackageDigest(target, asset.PackageDigest)
	}
}

func addCatalogPackageDigest(target map[string]struct{}, digest string) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" {
		target[digest] = struct{}{}
	}
}
