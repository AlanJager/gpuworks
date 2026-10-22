package accelerator

type nvidiaLogicalBackend struct{}

func (nvidiaLogicalBackend) ID() string { return BackendNVIDIALogical }
func (nvidiaLogicalBackend) Matches(vendor, _ string) bool {
	return NormalizeVendor(vendor) == VendorNVIDIA
}
func (nvidiaLogicalBackend) Maturity() Maturity       { return MaturityStable }
func (nvidiaLogicalBackend) RequiredAssets() []string { return []string{"libvgpu.so"} }
func (nvidiaLogicalBackend) Metadata() Metadata {
	return Metadata{Vendor: VendorNVIDIA, DisplayName: "NVIDIA dGPU", Compatibility: "NVIDIA CUDA GPU"}
}
func (nvidiaLogicalBackend) NeedsInstanceTeardown() bool { return true }

// The embedded libvgpu asset is validated at extraction time, so NVIDIA
// skips the behavioral probe.
func (nvidiaLogicalBackend) SkipsBehaviorProbe() bool { return true }

// HAMi-core reads the limit from the per-instance shared cache on every
// allocation check and memory query; the agent rewrites it there.
func (nvidiaLogicalBackend) LiveMemoryLimit() bool { return true }

func (nvidiaLogicalBackend) Profiles() []PartitionProfile { return nil }
