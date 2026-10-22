package accelerator

type ascendLogicalBackend struct{}

func (ascendLogicalBackend) ID() string { return BackendAscendLogical }
func (ascendLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorAscend
}
func (ascendLogicalBackend) Maturity() Maturity       { return MaturityExperimental }
func (ascendLogicalBackend) RequiredAssets() []string { return []string{"libvruntime.so"} }
func (ascendLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorAscend, DisplayName: "Ascend dGPU", Compatibility: "Host versions are advisory only; the container needs glibc ≥ 2.33"}
}
func (ascendLogicalBackend) NeedsInstanceTeardown() bool { return true }

// Ascend partition libraries require a newer glibc than driver-only host
// images typically ship, so the requirement is surfaced as a capability note.
func (ascendLogicalBackend) HostRequirementNotes() []string {
	return []string{"The inference image must provide glibc 2.33 or newer"}
}

var ascendProfiles = []PartitionProfile{
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "8.5", Architectures: []string{"x86_64"}, DisplayCompatibility: "CANN 8.5.x × x86_64", Verified: true, ChipFamilies: []string{"910B2", "910B2C"}},
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "8.5", Architectures: []string{"arm64"}, DisplayCompatibility: "CANN 8.5.x × ARM64", ChipFamilies: []string{"910B4"}},
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "9.0", Architectures: []string{"arm64"}, DisplayCompatibility: "CANN 9.0.x × ARM64", Verified: true, ChipFamilies: []string{"910B1", "910B4"}},
	{Backend: BackendAscendLogical, Vendor: VendorAscend, Framework: "cann", VersionPrefix: "9.0", Architectures: []string{"x86_64"}, DisplayCompatibility: "CANN 9.0.x × x86_64", ChipFamilies: []string{"910B"}},
}

func (ascendLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), ascendProfiles...)
}
