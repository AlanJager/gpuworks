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

// AMD is not a domestic accelerator (accelerator.IsDomestic), but it does
// ship a runtime asset bundle (accelerator.HasRuntimeAssetBundles), so the
// install must extract it the same way Ascend's is extracted above. This
// also exercises the real embed.FS entry for the AMD asset
// (embed_linux_amd64.go), which a test that only skips on a missing
// manifest would not catch if that one file's directive were missing.
func TestInstallDomesticRuntimeAssetsExtractsAMD(t *testing.T) {
	if _, err := files.ReadFile("bin/runtime-assets/manifest.json"); err != nil {
		t.Skip("runtime assets are not embedded in this binary")
	}
	if runtime.GOARCH != "amd64" {
		t.Skip("the AMD asset is linux/amd64 only")
	}
	root := t.TempDir()
	got, err := InstallDomesticRuntimeAssets(root, "amd")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Backend != "amd-dgpu" || got[0].Vendor != "amd" || got[0].Files != 1 {
		t.Fatalf("AMD asset groups = %+v", got)
	}
	asset := filepath.Join(root, "dgpu-runtime", "amd", "linux-amd64", "libvrocm.so")
	if info, err := os.Stat(asset); err != nil {
		t.Fatalf("installed lib %s: %v", asset, err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed lib %s is not executable: %v", asset, info.Mode())
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
	for _, key := range []string{"ascend-dgpu/amd64", "ascend-dgpu/arm64", "ppu-dgpu/amd64", "amd-dgpu/amd64"} {
		if !seen[key] {
			t.Fatalf("manifest lacks %s", key)
		}
	}
}
