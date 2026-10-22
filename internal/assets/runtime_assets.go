package assets

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

const DomesticRuntimeAssetVersion = "dgpu-runtime-v1"

type runtimeAssetManifest struct {
	Version string               `json:"version"`
	Bundles []runtimeAssetBundle `json:"bundles"`
}

type runtimeAssetBundle struct {
	ID           string             `json:"id"`
	Backend      string             `json:"backend"`
	Vendor       string             `json:"vendor"`
	PlatformArch string             `json:"platform_arch"`
	Files        []runtimeAssetFile `json:"files"`
}

type runtimeAssetFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type DomesticRuntimeAssetResult struct {
	Backend, Vendor, PlatformArch, AssetID string
	Files                                  int
}

// InstallDomesticRuntimeAssets extracts only packages matching the detected
// vendor and host architecture. NVIDIA and unknown nodes are strict no-ops.
func InstallDomesticRuntimeAssets(stateDir, vendor string) ([]DomesticRuntimeAssetResult, error) {
	if !accelerator.IsDomestic(vendor) {
		return nil, nil
	}
	manifest, err := readRuntimeAssetManifest()
	if err != nil {
		return nil, err
	}
	normalizedVendor := accelerator.NormalizeVendor(vendor)
	root := filepath.Join(stateDir, "dgpu-runtime")
	var results []DomesticRuntimeAssetResult
	for _, bundle := range manifest.Bundles {
		if bundle.Vendor != normalizedVendor || bundle.PlatformArch != runtime.GOARCH {
			continue
		}
		backend, ok := accelerator.Lookup(bundle.Backend)
		if !ok || !backend.Matches(bundle.Vendor, "") {
			return nil, fmt.Errorf("runtime asset bundle %s has invalid backend/vendor", bundle.ID)
		}
		for _, file := range bundle.Files {
			data, err := files.ReadFile("bin/runtime-assets/" + file.Path)
			if err != nil {
				return nil, fmt.Errorf("read domestic runtime asset %s: %w", file.Path, err)
			}
			if file.SHA256 == "" || !strings.EqualFold(fmt.Sprintf("%x", sha256.Sum256(data)), file.SHA256) {
				return nil, fmt.Errorf("domestic runtime asset %s sha256 mismatch", file.Path)
			}
			target := filepath.Join(root, filepath.FromSlash(file.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			if err := atomicWriteAsset(target, data, strings.HasSuffix(file.Path, ".so")); err != nil {
				return nil, fmt.Errorf("install domestic runtime asset %s: %w", file.Path, err)
			}
		}
		results = append(results, DomesticRuntimeAssetResult{Backend: bundle.Backend, Vendor: bundle.Vendor, PlatformArch: bundle.PlatformArch, AssetID: bundle.ID, Files: len(bundle.Files)})
	}
	if normalizedVendor == accelerator.VendorAscend && len(results) > 0 {
		if err := removeLegacyAscendAssetDirs(root, runtime.GOARCH); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func removeLegacyAscendAssetDirs(root, goarch string) error {
	archRoot := filepath.Join(root, "ascend", "linux-"+goarch)
	for _, version := range []string{"cann-8.5.1", "cann-9.0.0"} {
		legacy := filepath.Join(archRoot, version)
		if err := os.RemoveAll(legacy); err != nil {
			return fmt.Errorf("remove legacy Ascend runtime asset %s: %w", legacy, err)
		}
	}
	return nil
}

func readRuntimeAssetManifest() (runtimeAssetManifest, error) {
	data, err := files.ReadFile("bin/runtime-assets/manifest.json")
	if err != nil {
		return runtimeAssetManifest{}, err
	}
	var manifest runtimeAssetManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return runtimeAssetManifest{}, err
	}
	if manifest.Version != DomesticRuntimeAssetVersion {
		return runtimeAssetManifest{}, fmt.Errorf("unsupported domestic runtime asset package %q", manifest.Version)
	}
	return manifest, nil
}

func atomicWriteAsset(path string, data []byte, executable bool) error {
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
