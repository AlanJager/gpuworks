// Package accelerator defines the shared partition-backend registry used by
// both the control plane and the agent. The registry is deliberately explicit:
// adding a backend must be a visible, reviewable change here.
package accelerator

import "strings"

const (
	BackendNVIDIALogical = "nvidia-dgpu"
	BackendAscendLogical = "ascend-dgpu"
	BackendPPULogical    = "ppu-dgpu"

	EnvLDPreload            = "LD_PRELOAD"
	EnvLDLibraryPath        = "LD_LIBRARY_PATH"
	EnvNVIDIAVisibleDevices = "NVIDIA_VISIBLE_DEVICES"
	EnvCUDAVisibleDevices   = "CUDA_VISIBLE_DEVICES"

	DGPULibraryTarget        = "/gpuworks/libvgpu.so"
	DGPUCacheTarget          = "/gpuworks/cache"
	DGPUEnvMemoryCache       = "CUDA_DEVICE_MEMORY_SHARED_CACHE"
	DGPUEnvMemoryLimitPrefix = "CUDA_DEVICE_MEMORY_LIMIT_"
	DGPUEnvSMLimit           = "CUDA_DEVICE_SM_LIMIT"

	AscendLibraryTarget       = "/opt/gpuworks/partition/libvruntime.so"
	AscendPreloadTarget       = "/etc/ld.so.preload"
	AscendConfigTarget        = "/etc/enpu/vcann-rt/npu_info.config"
	AscendEnvDSMIHook         = "ENPU_DSMI_HOOK"
	AscendEnvVisibleDevices   = "ASCEND_VISIBLE_DEVICES"
	AscendEnvRTVisibleDevices = "ASCEND_RT_VISIBLE_DEVICES"
	AscendEnvNPUVisible       = "NPU_VISIBLE_DEVICES"

	PPUQuotaLibraryTarget   = "/opt/gpuworks/partition/hggc_quota.so"
	PPUHookLibraryTarget    = "/opt/gpuworks/partition/hgml_dlsym_hook.so"
	PPUEnvMemoryLimitPrefix = "HGGC_DEVICE_MEMORY_LIMIT_"
	PPUEnvSMLimitPrefix     = "HGGC_DEVICE_SM_LIMIT_"
	PPUEnvLedgerPath        = "HGGC_LEDGER_PATH"
)

type Maturity string

const (
	MaturityStable       Maturity = "stable"
	MaturityExperimental Maturity = "experimental"
)

// Metadata contains presentation data; it must not be used for admission.
type Metadata struct {
	Vendor        string
	DisplayName   string
	Compatibility string
}

// Backend is the definition of a partition backend. The agent-side container
// contract (mounts, env) lives in the engine's per-backend injectors — it is
// executable and asserted by tests, so it is not re-declared here. Admission is
// intentionally absent: all current backends share the scheduler's existing
// available-memory rule. Optional capabilities (runtime constraints, probe
// exemption, host requirement notes) are separate interfaces in
// capabilities.go.
type Backend interface {
	ID() string
	Matches(vendor, arch string) bool
	Maturity() Maturity
	RequiredAssets() []string
	Metadata() Metadata
	NeedsInstanceTeardown() bool
	// Profiles returns the backend's build-combination table for advisory
	// compatibility warnings; nil when the backend is built for none.
	Profiles() []PartitionProfile
}

var registry = []Backend{
	nvidiaLogicalBackend{},
	ascendLogicalBackend{},
	ppuLogicalBackend{},
}

func All() []Backend { return append([]Backend(nil), registry...) }

func Lookup(id string) (Backend, bool) {
	id = strings.TrimSpace(id)
	for _, backend := range registry {
		if backend.ID() == id {
			return backend, true
		}
	}
	return nil, false
}
