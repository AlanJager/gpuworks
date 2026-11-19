package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sleepHostBacking refuses a sleepable request whose engine would keep more
// host memory behind its GPU memory than one of its nodes has available
// (planning.SleepHostBackingRefusal). Evaluate and create both ask here, with
// the launch configuration they materialized, so a preview refuses what a
// create would.
//
// The GPU memory counted is one card's share: the engine's
// gpu_memory_utilization of the request's memory limit, or of the smallest
// card the request may run on. A request spanning several cards needs more, so
// what is refused here could not have started; a larger request that passes
// is left to start.
func (s *Service) sleepHostBacking(engine string, req *dto.DeployRequest, quick deploy.LaunchConfig, nodes ...*store.Node) error {
	if !req.Sleepable || s.d.HostMemory == nil {
		return nil
	}
	share := quick.GPUMemUtil
	if share <= 0 {
		share = deploy.DefaultGPUMemUtil(engine)
	}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		capacity := int64(req.VRAMLimitGiB * (1 << 30))
		if capacity <= 0 {
			for _, card := range DeployTargetCards(node, req.Placement) {
				if card.MemoryTotalBytes > 0 && (capacity == 0 || card.MemoryTotalBytes < capacity) {
					capacity = card.MemoryTotalBytes
				}
			}
		}
		if err := planning.SleepHostBackingRefusal(node, int64(share*float64(capacity)), s.d.HostMemory); err != nil {
			return err
		}
	}
	return nil
}
