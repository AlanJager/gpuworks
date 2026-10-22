package accelerator

import "strings"

type ppuLogicalBackend struct{}

func (ppuLogicalBackend) ID() string { return BackendPPULogical }
func (ppuLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorPPU
}
func (ppuLogicalBackend) Maturity() Maturity { return MaturityExperimental }
func (ppuLogicalBackend) RequiredAssets() []string {
	return []string{"hggc_quota.so", "hgml_dlsym_hook.so"}
}
func (ppuLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorPPU, DisplayName: "PPU dGPU", Compatibility: "HGGC 13.x × x86_64 · Docker"}
}
func (ppuLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The PPU ledger path has only been exercised under Docker; other runtimes
// stay unavailable until verified.
func (ppuLogicalBackend) RuntimeConstraint(driver string) string {
	if strings.EqualFold(strings.TrimSpace(driver), "docker") {
		return ""
	}
	return "PPU is currently verified on the Docker runtime only"
}

var ppuProfiles = []PartitionProfile{
	{Backend: BackendPPULogical, Vendor: VendorPPU, Framework: "hggc", VersionPrefix: "13", VersionOptional: true, Architectures: []string{"x86_64"}, DisplayCompatibility: "HGGC 13.x × x86_64", Verified: true, ChipFamilies: []string{"ZW810E", "Beethoven"}},
}

func (ppuLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), ppuProfiles...)
}
