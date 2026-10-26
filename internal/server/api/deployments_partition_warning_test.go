package api

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImagePartitionCompatibilityWarningUsesContainerCANNVersion(t *testing.T) {
	node := &store.Node{GPUVendor: "ascend", Arch: "arm64", GPUs: []store.GPU{{Vendor: "ascend", Name: "Ascend 910B4", ArchID: "910B4"}}}
	tests := []struct {
		version  string
		wantCode string
	}{
		{version: "9.0.0", wantCode: ""},
		{version: "8.5.1", wantCode: messages.WarningImagePartitionProfileUnverified},
		{version: "10.0.0", wantCode: messages.WarningImagePartitionProfileUnbuilt},
	}
	for _, tt := range tests {
		warnings := imagePartitionCompatibilityWarnings(&store.Image{Framework: "cann", FrameworkVersion: tt.version}, node)
		var gotCode string
		if len(warnings) > 0 {
			gotCode = warnings[0].Code
		}
		if gotCode != tt.wantCode {
			t.Errorf("CANN %s warnings = %v, want code %q", tt.version, warnings, tt.wantCode)
		}
	}
}

// The warning loop is backend-driven: any backend whose profiles serve the
// image framework warns, not just the first vendor that ever shipped them.
func TestImagePartitionCompatibilityWarningIsBackendDriven(t *testing.T) {
	ppuNode := &store.Node{GPUVendor: "ppu", Arch: "x86_64", GPUs: []store.GPU{{Vendor: "ppu", Name: "PPU ZW810E", ArchID: "ZW810E"}}}
	tests := []struct {
		name      string
		framework string
		version   string
		wantCode  string
	}{
		{name: "ppu hggc built", framework: "hggc", version: "13.1", wantCode: ""},
		{name: "ppu hggc outside profile", framework: "hggc", version: "14.0", wantCode: messages.WarningImagePartitionProfileUnbuilt},
		{name: "framework the backend does not serve", framework: "cann", version: "8.5.1", wantCode: ""},
	}
	for _, tt := range tests {
		warnings := imagePartitionCompatibilityWarnings(&store.Image{Framework: tt.framework, FrameworkVersion: tt.version}, ppuNode)
		var gotCode string
		if len(warnings) > 0 {
			gotCode = warnings[0].Code
		}
		if gotCode != tt.wantCode {
			t.Errorf("%s: warnings = %v, want code %q", tt.name, warnings, tt.wantCode)
		}
	}

	// NVIDIA has no build profiles; cuda images never produce a warning.
	nvidiaNode := &store.Node{GPUVendor: "nvidia", Arch: "x86_64", GPUs: []store.GPU{{Vendor: "nvidia", Name: "RTX 4090", ArchID: "sm_89"}}}
	if warnings := imagePartitionCompatibilityWarnings(&store.Image{Framework: "cuda", FrameworkVersion: "12.4"}, nvidiaNode); len(warnings) != 0 {
		t.Errorf("cuda image on NVIDIA produced warnings %v, want none", warnings)
	}
}

func TestImagePartitionCompatibilityWarningRequiresSupportedHygonSKU(t *testing.T) {
	image := &store.Image{Framework: "dtk", FrameworkVersion: "26.04.1"}
	node := &store.Node{GPUVendor: "hygon", Arch: "x86_64", GPUs: []store.GPU{{Vendor: "hygon", ArchID: "Z100"}}}
	warnings := imagePartitionCompatibilityWarnings(image, node)
	if len(warnings) != 1 || warnings[0].Code != messages.WarningImagePartitionProfileUnverified {
		t.Fatalf("Z100 warnings = %+v", warnings)
	}
	node.GPUs[0].ArchID = "K100AI"
	if warnings := imagePartitionCompatibilityWarnings(image, node); len(warnings) != 0 {
		t.Fatalf("unsupported Hygon SKU warnings = %+v", warnings)
	}
}
