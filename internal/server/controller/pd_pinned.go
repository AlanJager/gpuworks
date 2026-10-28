package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pinnedGPUsRefusal names, for a rank an operator pinned to cards that
// other deployments hold, those deployments - in place of the scheduler's
// count of free cards, which says nothing about the pin. A rank with no
// pinned cards, refused for another reason, or whose pinned card is held
// outside the platform or not healthy (the scheduler's reason says so)
// keeps the scheduler's reason.
func (c *Controller) pinnedGPUsRefusal(dep *store.Deployment, node *store.Node, pinned []int, refusal *scheduler.Reason) *scheduler.Reason {
	if len(pinned) == 0 || refusal == nil {
		return refusal
	}
	switch refusal.Code {
	case scheduler.ReasonInsufficientEmptyGPUs, scheduler.ReasonInsufficientSharedGPUs, scheduler.ReasonNoFreeGPU,
		scheduler.ReasonGPUsHeldOutsidePlatform, scheduler.ReasonNoHealthyGPU:
	default:
		return refusal
	}
	holders := map[int][]string{}
	if insts, err := c.store.InstancesForNode(node.ID); err == nil {
		names := map[uint]string{}
		for _, inst := range insts {
			if inst.DeploymentID == dep.ID || !planning.InstanceActive(inst.State) || inst.State == proto.InstStopped {
				continue
			}
			name, ok := names[inst.DeploymentID]
			if !ok {
				name = fmt.Sprintf("deployment %d", inst.DeploymentID)
				if other, err := c.store.GetDeployment(inst.DeploymentID); err == nil {
					name = other.Name
				}
				names[inst.DeploymentID] = name
			}
			for _, index := range parseInts(inst.GPUIndexes) {
				holders[index] = append(holders[index], name)
			}
		}
	}
	for _, gpu := range node.OwnedGPUs() {
		if slices.Contains(pinned, gpu.Index) && (gpu.HeldOutsidePlatform() || !gpu.CanTakeWork()) {
			// A card held outside the platform or not healthy: the
			// scheduler's own reason already says so.
			return refusal
		}
	}
	gpus := make([]string, 0, len(pinned))
	var held []string
	for _, index := range pinned {
		gpus = append(gpus, strconv.Itoa(index))
		if who := holders[index]; len(who) > 0 {
			slices.Sort(who)
			held = append(held, slices.Compact(who)...)
		}
	}
	if len(held) == 0 {
		// Free cards that still do not fit (memory, sharing): the scheduler's
		// own reason says how.
		return refusal
	}
	return &scheduler.Reason{Code: scheduler.ReasonPinnedGPUsUnavailable, Params: map[string]any{
		"node": node.DisplayName(), "gpus": strings.Join(gpus, ", "), "holders": strings.Join(slices.Compact(sortedStrings(held)), ", "),
	}}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}
