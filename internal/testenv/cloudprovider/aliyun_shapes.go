package cloudprovider

import "strings"

// AliyunShape is one ECS GPU instance type L4 cases may rent. The table is
// deliberately small and ordered: acceptance runs must not slide onto an
// unexpectedly expensive or incompatible card when the catalog changes.
type AliyunShape struct {
	InstanceType string
	// GPUModel is the short card name cases select by ("T4", "A10", "L20");
	// it is also a substring of what nvidia-smi prints for the card.
	GPUModel string
	GPUCount int
	VRAMMB   int
	// NoTensorParallel marks a multi-GPU shape the vLLM cases cannot use:
	// vLLM 0.17.1 (the cuda12.8 runner) fails TP=2 on it. Measured on 2xT4
	// (gn6i) 2026-10-26: CUDA graph capture dies with "CUDA error: out of
	// memory" on both cards, and with the agent's CUDA_VISIBLE_DEVICES=0,1 the
	// workers cannot initialise NVML. The shape is still fine for inventory
	// and provisioning cases.
	NoTensorParallel bool
}

// aliyunShapes lists the instance types in preference order, cheapest card
// first and, within a card, the smallest host first (a 12-vCPU slice is all
// the Qwen2-0.5B cases need). Survey of 2026-10-25: mainland spot stock exists
// for T4 (gn6i), A10 (gn7i-c32g1) and L20 (gn8is); 2-GPU spot is T4 and L20,
// 2-GPU A10 is on-demand only. V100 (gn6v, sm_70) is left out: the cuda12.8
// runner's PyTorch carries no kernels below sm_75, the same reason CompShare's
// V100S is excluded.
var aliyunShapes = []AliyunShape{
	{InstanceType: "ecs.gn6i-c8g1.2xlarge", GPUModel: "T4", GPUCount: 1, VRAMMB: 16384},
	{InstanceType: "ecs.gn6i-c16g1.4xlarge", GPUModel: "T4", GPUCount: 1, VRAMMB: 16384},
	{InstanceType: "ecs.gn6i-c24g1.6xlarge", GPUModel: "T4", GPUCount: 1, VRAMMB: 16384},
	{InstanceType: "ecs.gn6i-c24g1.12xlarge", GPUModel: "T4", GPUCount: 2, VRAMMB: 16384, NoTensorParallel: true},
	{InstanceType: "ecs.gn7i-c8g1.2xlarge", GPUModel: "A10", GPUCount: 1, VRAMMB: 24576},
	{InstanceType: "ecs.gn7i-c32g1.8xlarge", GPUModel: "A10", GPUCount: 1, VRAMMB: 24576},
	{InstanceType: "ecs.gn7i-2x.8xlarge", GPUModel: "A10", GPUCount: 2, VRAMMB: 24576},
	{InstanceType: "ecs.gn7i-c32g1.16xlarge", GPUModel: "A10", GPUCount: 2, VRAMMB: 24576},
	{InstanceType: "ecs.gn8is.2xlarge", GPUModel: "L20", GPUCount: 1, VRAMMB: 49152},
	{InstanceType: "ecs.gn8is.4xlarge", GPUModel: "L20", GPUCount: 1, VRAMMB: 49152},
	{InstanceType: "ecs.gn8is-2x.8xlarge", GPUModel: "L20", GPUCount: 2, VRAMMB: 49152},
}

// AliyunGPUModels is the card order cases walk, cheapest first.
func AliyunGPUModels() []string {
	var models []string
	seen := map[string]bool{}
	for _, shape := range aliyunShapes {
		if !seen[shape.GPUModel] {
			seen[shape.GPUModel] = true
			models = append(models, shape.GPUModel)
		}
	}
	return models
}

// AliyunShapes returns the shapes of one card with exactly gpuCount GPUs (an
// empty model matches every card), in preference order.
func AliyunShapes(gpuModel string, gpuCount int) []AliyunShape {
	var out []AliyunShape
	for _, shape := range aliyunShapes {
		if gpuModel != "" && !strings.EqualFold(shape.GPUModel, gpuModel) {
			continue
		}
		if gpuCount > 0 && shape.GPUCount != gpuCount {
			continue
		}
		out = append(out, shape)
	}
	return out
}

func aliyunShapeByType(instanceType string) (AliyunShape, bool) {
	for _, shape := range aliyunShapes {
		if shape.InstanceType == instanceType {
			return shape, true
		}
	}
	return AliyunShape{}, false
}

// AliyunGPUVRAMMB reports the memory per card of an Aliyun card name.
func AliyunGPUVRAMMB(gpuModel string) (int, bool) {
	for _, shape := range aliyunShapes {
		if strings.EqualFold(shape.GPUModel, gpuModel) {
			return shape.VRAMMB, true
		}
	}
	return 0, false
}

// AliyunTensorParallel reports whether a card's gpuCount-GPU shapes can run the
// multi-GPU vLLM cases (tensor parallel, distributed).
func AliyunTensorParallel(gpuModel string, gpuCount int) bool {
	for _, shape := range AliyunShapes(gpuModel, gpuCount) {
		if !shape.NoTensorParallel {
			return true
		}
	}
	return false
}
