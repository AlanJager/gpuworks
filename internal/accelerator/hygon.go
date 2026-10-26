package accelerator

import "strings"

type hygonLogicalBackend struct{}

func (hygonLogicalBackend) ID() string { return BackendHygonLogical }

// Only the Z100 has been exercised with a logical vdev quota. Unknown Hygon
// models must remain whole-card devices until their behavior is established.
func (hygonLogicalBackend) Matches(vendor, arch string) bool {
	return NormalizeVendor(vendor) == VendorHygon && strings.EqualFold(strings.TrimSpace(arch), "Z100")
}
func (hygonLogicalBackend) Maturity() Maturity { return MaturityExperimental }
func (hygonLogicalBackend) RequiredAssets() []string {
	return []string{"libamdhip64.so", "libhydmi.so", "libhydmi_mig.so", "librocm_smi64.so", "gpuworks-hygon-vdev-helper"}
}
func (hygonLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorHygon, DisplayName: "Hygon dGPU", Compatibility: "Z100 × x86_64 · Docker · memory-proportional CUs (experimental)"}
}
func (hygonLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The vdev configuration and device mapping are currently supported by the
// Docker injector only; other runtime paths require their own validation.
func (hygonLogicalBackend) RuntimeConstraint(driver string) string {
	if strings.EqualFold(strings.TrimSpace(driver), "docker") {
		return ""
	}
	return "Hygon dGPU currently requires the Docker runtime"
}

var hygonProfiles = []PartitionProfile{
	{Backend: BackendHygonLogical, Vendor: VendorHygon, Framework: "dtk", VersionPrefix: "26.04", VersionOptional: true, Architectures: []string{"x86_64"}, DisplayCompatibility: "Z100 × DTK 26.04.x × x86_64", ChipFamilies: []string{"Z100"}},
}

func (hygonLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), hygonProfiles...)
}
