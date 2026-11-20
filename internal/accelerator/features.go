package accelerator

// Feature is a platform capability requested beyond ordinary engine inference.
type Feature string

const (
	FeatureEngineSleep      Feature = "engine_sleep"
	FeatureMultiNode        Feature = "multi_node"
	FeaturePD               Feature = "prefill_decode_disaggregation"
	FeatureKVDiskTier       Feature = "kv_disk_tier"
	FeaturePipelineParallel Feature = "pipeline_parallelism"
	FeatureDataParallel     Feature = "data_parallelism"
	// FeatureExternalKVCache: a component outside the engine keeps the
	// instance's KV cache (a KV offload backend, ADR 0069). No vendor row
	// denies it; which vendors a backend runs on is the backend's own
	// allow-list, so this is only the requested-feature identity.
	FeatureExternalKVCache Feature = "external_kv_cache"
)

var Features = []Feature{FeatureEngineSleep, FeatureMultiNode, FeaturePD, FeatureKVDiskTier, FeaturePipelineParallel, FeatureDataParallel, FeatureExternalKVCache}

// A missing entry imposes no additional platform restriction. It does not
// promise engine or partition-backend support. Apple retains its existing
// all-features restriction; its ordinary shared-memory inference is separate.
type targetConstraint struct {
	unsupported     []Feature
	denyAll         bool
	runtime         hostRequirements
	explicitDevices bool
}

var targetConstraints = map[string]targetConstraint{
	VendorApple:     {denyAll: true},
	VendorKunlunxin: kunlunTargetConstraint,
	// AMD runs every launch feature NVIDIA does: multi-node, pipeline and
	// data parallelism ride RCCL over the same NCCL_* socket and IB
	// variables, vLLM sleep mode uses CuMemAllocator on HIP VMM, and PD uses
	// NIXL over UCX. What remains AMD-specific is the host runtime and the
	// explicit device selection below.
	VendorAMD: {
		runtime:         hostRequirements{os: "linux", arch: "amd64", drivers: []string{"docker", "nerdctl", "udocker", "kube"}},
		explicitDevices: true,
	},
}

func Supports(vendor string, feature Feature) bool {
	c := targetConstraints[NormalizeVendor(vendor)]
	if c.denyAll {
		return false
	}
	for _, blocked := range c.unsupported {
		if blocked == feature {
			return false
		}
	}
	return true
}

// SleepPoolOnHostMemory reports that an engine started with sleep mode keeps
// host memory behind the GPU memory it takes, for as long as it runs. Hygon's
// DTK does: vLLM's sleep allocator maps its pool through the CUDA
// virtual-memory calls, and DTK backs every byte mapped that way with host
// RAM. On a K100_AI (64 GiB) node with 62.4 GiB of RAM, host memory in use
// rose from 13 GiB to 57.5 GiB while a sleepable vLLM 0.18.1 allocated its
// 55 GiB KV cache, and the kernel killed the engine at each of three starts;
// the same launch without sleep mode held no such memory.
func SleepPoolOnHostMemory(vendor string) bool {
	return NormalizeVendor(vendor) == VendorHygon
}

// SharedByMemory is an admission policy, not a description of physical UMA.
// AMD continues to allocate whole cards even on hosts with shared system RAM.
func SharedByMemory(vendor string) bool {
	return NormalizeVendor(vendor) == VendorApple
}

func Unsupported(vendor string, want ...Feature) []Feature {
	var missing []Feature
	for _, feature := range want {
		if !Supports(vendor, feature) {
			missing = append(missing, feature)
		}
	}
	return missing
}

// RequiresDeviceSelection requires explicit allocation on both server and agent.
func RequiresDeviceSelection(vendor string) bool {
	return targetConstraints[NormalizeVendor(vendor)].explicitDevices
}

// RequiresKnownLaunchFeatures marks explicit partial launch restrictions. A
// blanket denyAll declaration (Apple) rejects requested features but preserves
// ordinary unknown-dialect inference; it does not claim an engine dialect.
func RequiresKnownLaunchFeatures(vendor string) bool {
	return len(targetConstraints[NormalizeVendor(vendor)].unsupported) > 0
}

// RequiresCollectiveInterface reports whether the target's collective runtime
// needs a real host interface rather than the single-process loopback default.
func RequiresCollectiveInterface(vendor string, nNodes, devices int) bool {
	return NormalizeVendor(vendor) == VendorKunlunxin && (nNodes > 1 || devices > 1)
}
