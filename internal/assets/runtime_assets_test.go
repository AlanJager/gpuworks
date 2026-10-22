package assets

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallDomesticRuntimeAssetsIsNoopForNVIDIA(t *testing.T) {
	root := t.TempDir()
	got, err := InstallDomesticRuntimeAssets(root, "nvidia")
	if err != nil || len(got) != 0 {
		t.Fatalf("NVIDIA install = %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "dgpu-runtime")); !os.IsNotExist(err) {
		t.Fatalf("NVIDIA asset directory exists: %v", err)
	}
}

func TestInstallDomesticRuntimeAssetsExtractsMatchingArch(t *testing.T) {
	if _, err := files.ReadFile("bin/runtime-assets/manifest.json"); err != nil {
		t.Skip("runtime assets are not embedded in this binary")
	}
	root := t.TempDir()
	for _, version := range []string{"cann-8.5.1", "cann-9.0.0"} {
		legacy := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH, version)
		if err := os.MkdirAll(legacy, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, "libvruntime.so"), []byte("legacy"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := InstallDomesticRuntimeAssets(root, "ascend")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Ascend asset groups = %+v, want one architecture-specific asset for %s", got, runtime.GOARCH)
	}
	asset := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH, "libvruntime.so")
	if _, err := os.Stat(asset); err != nil {
		t.Fatalf("installed lib %s: %v", asset, err)
	}
	legacy, err := filepath.Glob(filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH, "cann-*"))
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy versioned assets remain: %v, %v", legacy, err)
	}
	if got[0].Files != 1 || got[0].AssetID == "" {
		t.Fatalf("asset result = %+v", got[0])
	}
}

func TestRuntimeAssetManifestCarriesOneCheckedAssetPerBackendArchitecture(t *testing.T) {
	if _, err := files.ReadFile("bin/runtime-assets/manifest.json"); err != nil {
		t.Skip("runtime assets are not embedded in this binary")
	}
	manifest, err := readRuntimeAssetManifest()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, bundle := range manifest.Bundles {
		key := bundle.Backend + "/" + bundle.PlatformArch
		if seen[key] {
			t.Fatalf("duplicate runtime asset bundle %s", key)
		}
		seen[key] = true
		for _, file := range bundle.Files {
			if len(file.SHA256) != 64 {
				t.Fatalf("bundle %s file %s checksum = %q", bundle.ID, file.Path, file.SHA256)
			}
		}
	}
	for _, key := range []string{"ascend-dgpu/amd64", "ascend-dgpu/arm64", "ppu-dgpu/amd64"} {
		if !seen[key] {
			t.Fatalf("manifest lacks %s", key)
		}
	}
}
