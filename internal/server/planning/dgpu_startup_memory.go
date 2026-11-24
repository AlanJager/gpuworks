package planning

import (
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

// The default Decode graph capture exceeded the 8 GiB hard quota at 0.90
// on this runner. 0.88 was verified on the same card type (GWKS-345).
const qwen3DGPUStaticFractionMax = 0.88

func dgpuStartupMemoryRefusal(req LaunchRequest, plan LaunchPlan) *scheduler.Reason {
	p := plan.Params
	if req.Engine != deploy.EngineSGLang || req.PD != nil || p.NNodes > 1 ||
		p.PartitionBackend != accelerator.BackendAscendLogical || p.VRAMLimitBytes != 8<<30 ||
		p.Vendor != accelerator.VendorAscend || p.ArchID != "910B4" ||
		p.AssignedGPUs != 1 || p.TensorParallel != 1 || p.PipelineParallel > 1 || p.DataParallel > 1 ||
		req.Selection.ModelID != "qwen/qwen3-0.6b" || req.Selection.SpecName != "bf16-sglang-ascend-25218b07" ||
		pullref.NormalizeRef(req.Source.Image) != "docker.io/gpuworks/gpuworks-runner:cann9.0-910b-sglang0.5.18-r2" ||
		plan.Resolved.CommandMode != string(deploy.CommandEngine) {
		return nil
	}
	args, _, ok := deploy.LaunchArgs(plan.Resolved)
	dtype, _ := deploy.LaunchFlag(args, "--dtype")
	if !ok || (dtype != "bfloat16" && dtype != "bf16") || !usesDefaultSGLangDecodeGraph(args) {
		return nil
	}
	raw, _ := deploy.LaunchFlag(args, "--mem-fraction-static")
	fraction, err := strconv.ParseFloat(raw, 64)
	if err != nil || !(fraction > qwen3DGPUStaticFractionMax) {
		return nil
	}
	return &scheduler.Reason{
		Code: scheduler.ReasonDGPUStartupMemory,
		Message: fmt.Sprintf("The 8 GiB dGPU slice needs memory for Decode graph capture. Lower GPU memory utilization from %s to %.2f or less.",
			raw, qwen3DGPUStaticFractionMax),
		Params: map[string]any{"quota_gib": 8, "utilization": fraction, "max_utilization": qwen3DGPUStaticFractionMax},
	}
}

func usesDefaultSGLangDecodeGraph(args []string) bool {
	for _, arg := range args {
		flag, _, _ := strings.Cut(arg, "=")
		switch flag {
		case "--disable-cuda-graph", "--disable-cuda-graph-padding", "--cuda-graph-bs", "--cuda-graph-max-bs":
			return false
		}
	}
	return true
}
