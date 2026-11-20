package accelerator

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
	return Metadata{Vendor: VendorPPU, DisplayName: "PPU dGPU", Compatibility: "HGGC 13.x × x86_64 · Docker / Kubernetes"}
}
func (ppuLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The PPU ledger path has only been exercised under Docker and kube; other
// runtimes stay unavailable until verified. Under Docker the cross-process
// ledger is backed by a file in the instance's partition state directory.
// A Kubernetes agent has a private /dev/shm, so it pre-creates the ledger
// on the host for the workload pod to mount (see partition_ppu.go).
func (ppuLogicalBackend) SupportedRuntimes() []string { return []string{"docker", "kube"} }

// The shim's usage region carries what the slice is charged on each card. Its
// compute controller measures utilization only on a card whose compute is
// capped below the whole card, so an uncapped slice has no compute figure.
func (ppuLogicalBackend) SliceUsage(smLimitPct int) (memory, compute bool) {
	return true, smLimitPct > 0 && smLimitPct < 100
}

var ppuProfiles = []PartitionProfile{
	{Backend: BackendPPULogical, Vendor: VendorPPU, Framework: "hggc", VersionPrefix: "13", VersionOptional: true, Architectures: []string{"x86_64"}, DisplayCompatibility: "HGGC 13.x × x86_64", Verified: true, ChipFamilies: []string{"ZW810E", "Beethoven"}},
}

func (ppuLogicalBackend) Profiles() []PartitionProfile {
	return append([]PartitionProfile(nil), ppuProfiles...)
}
