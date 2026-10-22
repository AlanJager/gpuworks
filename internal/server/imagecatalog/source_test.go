package imagecatalog

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCandidatesBridgeVendorAndSeriesCompatibilityRules(t *testing.T) {
	st, err := store.Open("file:imagecatalog_candidates_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Image{}).Where("1 = 1").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	entries := []store.Image{
		{ImageRef: "catalog/cuda:12.4", Name: "cuda", Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.4", Enabled: true},
		{ImageRef: "catalog/cuda:12.6", Name: "cuda", Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.6", Enabled: true, Deprecated: true},
		{ImageRef: "catalog/cuda:13.0", Name: "cuda", Engine: "vllm", Framework: "cuda", FrameworkVersion: "13.0", Enabled: true},
		{ImageRef: "catalog/ascend:8.1", Name: "ascend", Engine: "vllm", Framework: "cann", FrameworkVersion: "8.1", Enabled: true},
	}
	for i := range entries {
		if _, err := st.UpsertImage(&entries[i]); err != nil {
			t.Fatalf("upsert %s: %v", entries[i].ImageRef, err)
		}
	}
	candidates, err := Candidates(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) < len(entries) {
		t.Fatalf("candidates = %d, want at least %d", len(candidates), len(entries))
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		seen[candidate.ImageRef] = true
	}
	for _, entry := range entries {
		if !seen[entry.ImageRef] {
			t.Fatalf("candidate %s missing", entry.ImageRef)
		}
	}
	got := Resolve(st, deploy.ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.5", DriverVersion: "550.54.15", PlatformArch: "amd64"})
	if got.Image != "catalog/cuda:12.4" || got.Source != "catalog" {
		t.Fatalf("compatible resolution = %+v", got)
	}
	got = Resolve(st, deploy.ImageTarget{Engine: "vllm", Vendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.1", DriverVersion: "580.00", PlatformArch: "amd64"})
	if got.Image != "catalog/cuda:13.0" {
		t.Fatalf("major-version resolution = %+v", got)
	}
	got = Resolve(st, deploy.ImageTarget{Engine: "vllm", Vendor: "ascend", Framework: "cann", FrameworkVersion: "8.1", PlatformArch: "arm64", Override: "catalog/cuda:12.4"})
	if got.Image != "catalog/cuda:12.4" || len(got.Warnings) == 0 {
		t.Fatalf("manual incompatible override = %+v", got)
	}
}
