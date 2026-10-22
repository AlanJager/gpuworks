package scheduler

import "gpuworks.ai/gpuworks/internal/accelerator"

// runFilters applies the hard admission filters in order. Returns the first
// failure reason, or ok=true.
func runFilters(req Request, node NodeSnapshot) (bool, Reason) {
	if node.ServingUnavailable {
		return false, Reason{Code: ReasonServingUnavailable}
	}
	if missing := accelerator.Unsupported(node.Vendor, req.Features...); len(missing) > 0 {
		return false, Reason{Code: featureReason[missing[0]], Params: map[string]any{"vendor": accelerator.VendorBrand(node.Vendor)}}
	}
	if req.StorageRefusal != nil {
		return false, *req.StorageRefusal
	}
	if !node.DtypeSupported {
		if node.DtypeReason != "" {
			return false, Reason{Code: ReasonDtypeUnsupportedDetail, Params: map[string]any{"detail": node.DtypeReason}}
		}
		return false, Reason{Code: ReasonDtypeUnsupported}
	}
	if req.PerGPULimitBytes > 0 {
		partitionReady := false
		for _, gpu := range node.GPUs {
			if gpu.Schedulable && contains(gpu.PartitionBackends, req.PartitionBackend) {
				partitionReady = true
				break
			}
		}
		if req.PartitionBackend == "" || !partitionReady {
			return false, Reason{Code: ReasonPartitionUnavailable}
		}
	}
	if req.DiskNeedBytes > 0 && node.DiskAvailBytes > 0 && node.DiskAvailBytes < req.DiskNeedBytes {
		return false, Reason{Code: ReasonInsufficientDisk}
	}
	if req.RuntimeDiskNeedBytes > 0 && node.RuntimeDiskAvailBytes > 0 && node.RuntimeDiskAvailBytes < req.RuntimeDiskNeedBytes {
		return false, Reason{Code: ReasonInsufficientRuntimeDisk}
	}
	if countSchedulable(node.GPUs) == 0 {
		return false, Reason{Code: noSchedulableGPUReason(node.GPUs)}
	}
	if countFree(node.GPUs) == 0 {
		return false, Reason{Code: ReasonNoFreeGPU}
	}
	return true, Reason{}
}

// featureReason names each platform feature's refusal.
var featureReason = map[accelerator.Feature]string{
	accelerator.FeatureEngineSleep: ReasonSleepUnsupportedOnNode,
	accelerator.FeatureMultiNode:   ReasonMultiNodeUnsupportedOnNode,
	accelerator.FeatureKVDiskTier:  ReasonKVDiskUnsupportedOnNode,
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countFree(gpus []GPU) int {
	n := 0
	for _, g := range gpus {
		if g.Schedulable && g.Available() > 0 {
			n++
		}
	}
	return n
}

func countSchedulable(gpus []GPU) int {
	n := 0
	for _, g := range gpus {
		if g.Schedulable {
			n++
		}
	}
	return n
}

// noSchedulableGPUReason says why a node none of whose cards takes work
// cannot take this request, naming what an operator would act on. A card
// held outside the platform is named before a fault: freeing it is the
// quicker remedy, and it is not something a health alert would show.
func noSchedulableGPUReason(gpus []GPU) string {
	if len(gpus) == 0 {
		return ReasonNoOwnedGPU
	}
	for _, g := range gpus {
		if g.HeldOutsidePlatform {
			return ReasonGPUsHeldOutsidePlatform
		}
	}
	return ReasonNoHealthyGPU
}

// usableGPUs returns the GPUs that can host the request at the given per-GPU
// need. A need of zero asks for structural eligibility only, which is what the
// planner starts from before a placement contract has had its say.
func usableGPUs(gpus []GPU, req Request, need int64) []GPU {
	var out []GPU
	for _, g := range gpus {
		if g.UsableFor(req, need) {
			out = append(out, g)
		}
	}
	return out
}
