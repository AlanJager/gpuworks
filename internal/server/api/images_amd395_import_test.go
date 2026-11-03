package api

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAMD395ImageImportAddsCustomImagesWithoutReplacingOfficialCatalog(t *testing.T) {
	run := newImageManifestRun(t)
	const officialRef = "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2"
	beforeOfficial, err := run.store.ImageByRef(officialRef)
	if err != nil {
		t.Fatalf("seeded NVIDIA official image %q: %v", officialRef, err)
	}
	var officialCountBefore int64
	if err := run.store.DB().Model(&store.Image{}).Where("source = ?", "official").Count(&officialCountBefore).Error; err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile("../../../build/inference-templates/amd395/image-import.json")
	if err != nil {
		t.Fatal(err)
	}
	// The shipped source is deliberately unimportable. Model the output of the
	// explicit, exact-image-ID-gated mapping step with fixture-only refs.
	mapped := string(body)
	for source, ref := range map[string]string{
		"__AMD395_VLLM_IMAGE_MAPPING_REQUIRED__:required":      "example.invalid/amd395-vllm:r2",
		"__AMD395_SGLANG_R2_IMAGE_MAPPING_REQUIRED__:required": "example.invalid/amd395-sglang:r2",
		"__AMD395_SGLANG_R5_IMAGE_MAPPING_REQUIRED__:required": "example.invalid/amd395-sglang:r5",
	} {
		mapped = strings.ReplaceAll(mapped, source, ref)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		rec := run.do(http.MethodPost, "/api/v1/images/import", "application/json", mapped)
		if rec.Code != http.StatusOK {
			t.Fatalf("import attempt %d = %d %s", attempt, rec.Code, rec.Body.String())
		}
	}

	expectedImages := []struct {
		ref, engine, framework, frameworkVersion string
	}{
		{"example.invalid/amd395-vllm:r2", "vllm", "rocm", "7.2.3"},
		{"example.invalid/amd395-sglang:r2", "sglang", "rocm", "7.2.4"},
		{"example.invalid/amd395-sglang:r5", "sglang", "rocm", "7.2.4"},
	}
	for _, tc := range expectedImages {
		image, err := run.store.ImageByRef(tc.ref)
		if err != nil {
			t.Fatalf("AMD image %q missing: %v", tc.ref, err)
		}
		if image.Source != "custom" || image.Engine != tc.engine || image.Framework != tc.framework || image.FrameworkVersion != tc.frameworkVersion || image.Arch != "amd64" {
			t.Fatalf("AMD image import facts = %+v", image)
		}
	}
	var amdCount int64
	if err := run.store.DB().Model(&store.Image{}).Where("source = ? AND image_ref LIKE ?", "custom", "example.invalid/amd395-%").Count(&amdCount).Error; err != nil {
		t.Fatal(err)
	}
	if amdCount != int64(len(expectedImages)) {
		t.Fatalf("AMD custom image count = %d, want %d after repeated import", amdCount, len(expectedImages))
	}
	var importedRefs []string
	if err := run.store.DB().Model(&store.Image{}).Where("source = ? AND image_ref LIKE ?", "custom", "example.invalid/amd395-%").Order("image_ref").Pluck("image_ref", &importedRefs).Error; err != nil {
		t.Fatal(err)
	}
	wantRefs := make(map[string]bool, len(expectedImages))
	for _, image := range expectedImages {
		wantRefs[image.ref] = true
	}
	for _, ref := range importedRefs {
		if !wantRefs[ref] {
			t.Errorf("unexpected AMD custom image imported: %q", ref)
		}
		delete(wantRefs, ref)
	}
	for ref := range wantRefs {
		t.Errorf("expected AMD custom image %q was not imported", ref)
	}

	afterOfficial, err := run.store.ImageByRef(officialRef)
	if err != nil || afterOfficial.Source != beforeOfficial.Source || afterOfficial.Engine != beforeOfficial.Engine || afterOfficial.Framework != beforeOfficial.Framework || afterOfficial.Deprecated {
		t.Fatalf("official NVIDIA image changed after additive import: before=%+v after=%+v err=%v", beforeOfficial, afterOfficial, err)
	}
	var officialCountAfter int64
	if err := run.store.DB().Model(&store.Image{}).Where("source = ?", "official").Count(&officialCountAfter).Error; err != nil {
		t.Fatal(err)
	}
	if officialCountAfter != officialCountBefore {
		t.Fatalf("official image count changed %d -> %d", officialCountBefore, officialCountAfter)
	}

	if _, err := imagecatalog.SyncSeed(run.store); err != nil {
		t.Fatalf("sync embedded official catalog: %v", err)
	}
	for _, expected := range expectedImages {
		image, err := run.store.ImageByRef(expected.ref)
		if err != nil || image.Source != "custom" {
			t.Fatalf("AMD custom image %q did not survive SyncSeed: %+v err=%v", expected.ref, image, err)
		}
	}
	if image, err := run.store.ImageByRef(officialRef); err != nil || image.Source != "official" {
		t.Fatalf("official image %q did not survive SyncSeed: %+v err=%v", officialRef, image, err)
	}
}
