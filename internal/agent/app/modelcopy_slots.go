package app

import (
	"gpuworks.ai/gpuworks/internal/proto"
)

// A hung filesystem call retains its task slot until the worker actually
// returns. writerSlotAvailableLocked also caps blocked copy workers per node.
const maxConcurrentModelCopies = 4

// writerSlotAvailableLocked reports whether a writer for key may start at
// target: no other writer owns an overlapping path and, for a copy, the node is
// below the copy limit. n.dlMu is held.
func (n *NodeRuntime) writerSlotAvailableLocked(key, target string, copyWriter bool) bool {
	copies := 0
	for otherKey, active := range n.downloading {
		if otherKey == key || pathWithin(target, active.target) || pathWithin(active.target, target) {
			return false
		}
		if active.copy {
			copies++
		}
	}
	return !copyWriter || copies < maxConcurrentModelCopies
}

// supersedes reports whether task replaces the transfer that is running
// under its key. The identity of a transfer is its TransferID: a copy with
// another ID, or a download the server prepared anew (a new TransferID), is a
// different transfer. A download whose ID is the same or unknown (a server that
// predates download IDs sends none) is the running one, whatever else about the
// task changed: its rate limit and addresses are read when it starts, and
// restarting a download for them would only throw away its progress.
func supersedes(active *modelDownload, task proto.ModelFileTask) bool {
	if active.copy {
		return active.transferID != task.TransferID
	}
	return task.Acquisition == proto.ModelAcquireDownload && task.TransferID != "" && active.transferID != task.TransferID
}

// copyTaskRunning reports whether the key already has a writer that decides
// task: the task is then neither started nor reported here. A writer of
// another transfer is canceled and the task waits for it to return; a copy
// waits behind a download of the same key without canceling it.
func (n *NodeRuntime) copyTaskRunning(task proto.ModelFileTask, retry bool) bool {
	n.dlMu.Lock()
	if stopped, ok := n.stoppedCopies[task.Key]; ok {
		if stopped == task.TransferID {
			n.dlMu.Unlock()
			n.settleParkedModelFile(task, retry)
			return true
		}
		delete(n.stoppedCopies, task.Key)
	}
	active := n.downloading[task.Key]
	if active != nil && (supersedes(active, task) || (!active.copy && task.Acquisition == proto.ModelAcquireCopy)) {
		if supersedes(active, task) {
			active.cancel()
		}
		parkWriterModelFileForTask(n, task, retry)
		n.dlMu.Unlock()
		return true
	}
	n.dlMu.Unlock()
	if active != nil {
		n.settleParkedModelFile(task, retry)
	}
	return active != nil
}
