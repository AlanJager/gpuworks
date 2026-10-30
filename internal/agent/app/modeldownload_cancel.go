package app

import "gpuworks.ai/gpuworks/internal/proto"

// A full replay is authoritative even for transfers started before disconnect.
func (n *NodeRuntime) withdrawUnlistedDownloads(tasks []proto.ModelFileTask) {
	wanted := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		wanted[task.Key] = true
	}
	n.dlMu.Lock()
	defer n.dlMu.Unlock()
	for key, active := range n.downloading {
		if !wanted[key] {
			if active.transferID != "" {
				n.fenceDownloadLocked(key, active.transferID)
			}
			active.cancel()
		}
	}
}

// Cancellation is an attempt-level tombstone, not the newest task's state.
// A newer retry must not erase it: an older snapshot can arrive after that retry.
// The caller holds dlMu.
func (n *NodeRuntime) fenceDownloadLocked(key, transferID string) {
	if n.stoppedDownloads == nil {
		n.stoppedDownloads = map[string]map[string]bool{}
	}
	if n.stoppedDownloads[key] == nil {
		n.stoppedDownloads[key] = map[string]bool{}
	}
	n.stoppedDownloads[key][transferID] = true
}

func (n *NodeRuntime) downloadTaskStopped(task proto.ModelFileTask) bool {
	n.dlMu.Lock()
	defer n.dlMu.Unlock()
	return n.stoppedDownloads[task.Key][task.TransferID]
}
