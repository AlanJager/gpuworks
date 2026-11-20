//go:build linux && dqlite

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const materialManifestVersion = 1
const maxMaterialManifestBytes = 16 << 20
const maxMaterialManifestFiles = 100000

// materialManifest is the portable mapping from content-addressed bytes to
// application consumer paths. A receiver must validate it against a current
// admitted source and must not install another member's private identity.
type materialManifest struct {
	Version        int                    `json:"version"`
	ClusterID      string                 `json:"cluster_id"`
	SourceMemberID uint64                 `json:"source_member_id"`
	SourceKey      string                 `json:"source_key"`
	Classes        []MaterialClass        `json:"classes"`
	Files          []materialManifestFile `json:"files"`
}

type materialManifestFile struct {
	Class  MaterialClass `json:"class"`
	Path   string        `json:"path"`
	SHA256 string        `json:"sha256"`
	Bytes  int64         `json:"bytes"`
}

func buildMaterialManifest(dataDir string, inventory MaterialInventory, local map[string]string, identity store.ClusterIdentity, memberID uint64, key string) ([]byte, error) {
	root, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	manifest := materialManifest{Version: materialManifestVersion, ClusterID: identity.ClusterID, SourceMemberID: memberID, SourceKey: key, Classes: slices.Clone(inventory.Classes)}
	for _, file := range inventory.Files {
		rel, err := filepath.Rel(root, file.Path)
		if err != nil {
			return nil, err
		}
		canonical := local[file.SHA256]
		info, err := os.Lstat(canonical)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s material has no durable canonical bytes", file.Class)
		}
		manifest.Files = append(manifest.Files, materialManifestFile{Class: file.Class, Path: filepath.ToSlash(rel), SHA256: file.SHA256, Bytes: info.Size()})
	}
	if err := validateMaterialManifest(manifest, identity, memberID, key); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil || len(raw) > maxMaterialManifestBytes {
		return nil, errors.New("material manifest exceeds its limit")
	}
	return raw, nil
}

func validateMaterialManifest(m materialManifest, identity store.ClusterIdentity, memberID uint64, key string) error {
	if m.Version != materialManifestVersion || m.ClusterID == "" || m.ClusterID != identity.ClusterID || m.SourceMemberID == 0 || m.SourceMemberID != memberID || !validMaterialDigest(m.SourceKey) || m.SourceKey != key {
		return errors.New("material manifest source identity differs")
	}
	if len(m.Classes) != len(requiredMaterialClasses) || !slices.Equal(m.Classes, requiredMaterialClasses) || len(m.Files) == 0 || len(m.Files) > maxMaterialManifestFiles {
		return errors.New("material manifest has incomplete coverage")
	}
	lastClass, lastPath := MaterialClass(""), ""
	seenPaths := make(map[string]materialManifestFile, len(m.Files))
	seenDigests := make(map[string]int64, len(m.Files))
	for _, file := range m.Files {
		if !validMaterialDigest(file.SHA256) || file.Bytes <= 0 || file.Bytes > maxMaterialReplicaBytes || !validMaterialConsumerPath(file.Class, file.Path) {
			return fmt.Errorf("material manifest has invalid %s path or bytes", file.Class)
		}
		if file.Class < lastClass || (file.Class == lastClass && file.Path <= lastPath) {
			return errors.New("material manifest files are not sorted and unique")
		}
		if prior, exists := seenPaths[file.Path]; exists && (prior.SHA256 != file.SHA256 || prior.Bytes != file.Bytes || !(prior.Class == MaterialCatalogPackage && file.Class == MaterialRuntimeAsset)) {
			return errors.New("material manifest assigns contradictory bytes or classes to a path")
		}
		if size, exists := seenDigests[file.SHA256]; exists && size != file.Bytes {
			return errors.New("material manifest assigns different sizes to one digest")
		}
		seenPaths[file.Path] = file
		seenDigests[file.SHA256] = file.Bytes
		lastClass, lastPath = file.Class, file.Path
	}
	return nil
}

func parseMaterialManifest(raw []byte, identity store.ClusterIdentity, memberID uint64, key string) (materialManifest, error) {
	var manifest materialManifest
	if len(raw) == 0 || len(raw) > maxMaterialManifestBytes {
		return manifest, errors.New("material manifest exceeds its limit")
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return materialManifest{}, err
	}
	if err := validateMaterialManifest(manifest, identity, memberID, key); err != nil {
		return materialManifest{}, err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonical, raw) {
		return materialManifest{}, errors.New("material manifest is not canonical")
	}
	return manifest, nil
}

func validMaterialConsumerPath(class MaterialClass, name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	under := func(root string) bool { return strings.HasPrefix(name, root+"/") && len(name) > len(root)+1 }
	switch class {
	case MaterialMasterSecret:
		return name == "master.key"
	case MaterialMemberIdentity:
		return under(enrollmentDirectory) || under("gpuworks.db.join/materials") || under("gpuworks.db.dqlite/.transport") || name == "gpuworks.db.join/member.key" || name == "gpuworks.db.join/config.json" || name == "gpuworks.db.join/materials.commit"
	case MaterialCatalogPackage:
		return under("catalog-packages")
	case MaterialRuntimeAsset:
		return under("catalog-packages") && strings.Contains(name, "/assets/")
	case MaterialOfflineImage:
		return under("images")
	case MaterialBenchmarkInput, MaterialBenchmarkDetail:
		return under("benchmark-artifacts")
	case MaterialModelLogo:
		return under("model-logos")
	case MaterialSSHHostKeys:
		return name == "ssh-hostkeys.json"
	case MaterialAgentOverride:
		return under("dl")
	default:
		return false
	}
}
