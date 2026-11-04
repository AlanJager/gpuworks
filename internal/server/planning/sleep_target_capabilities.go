package planning

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SleepTargetNodeReader supplies the host card inventory needed to apply sleep
// capability policy to the GPUs selected by stored instances.
type SleepTargetNodeReader interface {
	NodeByID(id uint) (*store.Node, error)
}

// UnsupportedSleepTarget refuses a new sleep order when any selected GPU of
// an instance holding sleep intent lacks engine-sleep support. It uses the
// persisted GPU selection, falling back to the same selection in the desired
// state for rows created before GPUIndexes was populated. Wake admission is a
// separate policy and must not call this helper, so already-sleeping instances
// on older unsupported targets can recover. The node inventory read is needed
// to resolve each selected physical card's vendor; node-level vendor would
// reject a mixed node when only a supported card was selected.
func UnsupportedSleepTarget(engine string, insts []store.Instance, nodes SleepTargetNodeReader) (*SleepRefusal, error) {
	byNode := make(map[uint]*store.Node)
	for i := range insts {
		inst := &insts[i]
		if !store.HoldsSleepIntent(inst) {
			continue
		}
		node := byNode[inst.NodeID]
		var err error
		if node == nil {
			node, err = nodes.NodeByID(inst.NodeID)
			if err != nil {
				return nil, fmt.Errorf("load sleep target node %d: %w", inst.NodeID, err)
			}
			if node == nil {
				return nil, fmt.Errorf("load sleep target node %d: no node returned", inst.NodeID)
			}
			byNode[inst.NodeID] = node
		}
		requiresSelection := !accelerator.Supports(node.GPUVendor, accelerator.FeatureEngineSleep)
		for _, gpu := range node.OwnedGPUs() {
			if !accelerator.Supports(node.VendorOf(gpu), accelerator.FeatureEngineSleep) {
				requiresSelection = true
				break
			}
		}
		if !requiresSelection {
			continue
		}
		indexes, err := sleepTargetGPUIndexes(inst)
		if err != nil {
			return nil, err
		}
		if len(indexes) == 0 {
			return nil, sleepTargetError(inst, "GPU selection is empty")
		}
		for _, index := range indexes {
			gpu, ok := node.GPUByIndex(index)
			if !ok {
				return nil, sleepTargetError(inst, fmt.Sprintf("GPU index %d is not in inventory", index))
			}
			vendor := node.VendorOf(gpu)
			if accelerator.NormalizeVendor(vendor) == "" {
				return nil, sleepTargetError(inst, fmt.Sprintf("GPU index %d has unknown vendor", index))
			}
			if !accelerator.Supports(vendor, accelerator.FeatureEngineSleep) {
				return &SleepRefusal{
					Code: messages.ErrorAcceleratorFeatureUnsupported,
					Params: map[string]any{
						"engine": engine, "vendor": accelerator.VendorBrand(vendor),
						"feature": string(accelerator.FeatureEngineSleep),
					},
				}, nil
			}
		}
	}
	return nil, nil
}

func sleepTargetGPUIndexes(inst *store.Instance) ([]int, error) {
	var indexes []int
	if err := json.Unmarshal([]byte(inst.GPUIndexes), &indexes); err == nil && len(indexes) > 0 {
		return indexes, nil
	}
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &desired); err != nil {
		return nil, sleepTargetError(inst, "cannot decode desired GPU selection: "+err.Error())
	}
	return desired.GPUIndexes, nil
}

func sleepTargetError(inst *store.Instance, detail string) error {
	return messages.NewProductError(messages.ErrorSleepTargetUnresolved, "cannot verify sleep target GPU selection", map[string]any{"instance": inst.UUID, "node": inst.NodeID, "detail": detail})
}
