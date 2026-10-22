package app

import "gpuworks.ai/gpuworks/internal/proto"

// A hung filesystem call retains its task slot until the worker actually
// returns. Together with copyWorkers this caps blocked copy workers per node.
const maxConcurrentModelCopies = 4

func (n *NodeRuntime) copySlotAvailableLocked(target string) bool {
	copies := 0
	for _, active := range n.downloading {
		if pathWithin(target, active.target) || pathWithin(active.target, target) {
			return false
		}
		if active.copy {
			copies++
		}
	}
	return copies < maxConcurrentModelCopies
}

func (n *NodeRuntime) copyTaskRunning(task proto.ModelFileTask) bool {
	n.dlMu.Lock()
	if stopped, ok := n.stoppedCopies[task.Key]; ok {
		if stopped == task.TransferID {
			n.dlMu.Unlock()
			return true
		}
		delete(n.stoppedCopies, task.Key)
	}
	active := n.downloading[task.Key]
	if active != nil && active.copy && active.transferID != task.TransferID {
		active.cancel()
	}
	n.dlMu.Unlock()
	if active != nil && active.copy && active.transferID != task.TransferID {
		n.parkTask(task)
	}
	return active != nil
}
