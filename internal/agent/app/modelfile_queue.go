package app

import (
	"context"
	"reflect"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Parked model-file tasks.
//
// A task that cannot run yet waits in one of two queues of storageRuntime:
// waiting (its storage is not ready) or writerWaiting (another writer owns an
// overlapping path, or the copy limit is reached). Each queue holds the latest
// task for a key; a newer task from the server replaces the parked one. That is
// all the agent remembers about which task is current. Which report is current
// is the server's call: every status carries the task's TransferID and
// store.UpdateModelFileStatus drops the ones that name a superseded transfer.
//
// A queued task is retried from a snapshot (the storage check, or the return of
// a path owner). The snapshot only acts while its task is still the key's parked
// task, which is how eviction, a withdrawal or a newer task fence a retry that
// was already in flight.

func sameModelFileTask(a, b proto.ModelFileTask) bool {
	return reflect.DeepEqual(a, b)
}

// parkedLocked returns the task parked for key in either queue. s.mu is held.
func (s *storageRuntime) parkedLocked(key string) (proto.ModelFileTask, bool) {
	if task, ok := s.waiting[key]; ok {
		return task, true
	}
	task, ok := s.writerWaiting[key]
	return task, ok
}

func (n *NodeRuntime) parkedModelFile(key string) (proto.ModelFileTask, bool) {
	if n.storage == nil {
		return proto.ModelFileTask{}, false
	}
	n.storage.mu.RLock()
	defer n.storage.mu.RUnlock()
	return n.storage.parkedLocked(key)
}

func (n *NodeRuntime) parkedModelFiles() []proto.ModelFileTask {
	if n.storage == nil {
		return nil
	}
	n.storage.mu.RLock()
	defer n.storage.mu.RUnlock()
	tasks := make([]proto.ModelFileTask, 0, len(n.storage.waiting)+len(n.storage.writerWaiting))
	for _, task := range n.storage.waiting {
		tasks = append(tasks, task)
	}
	for _, task := range n.storage.writerWaiting {
		tasks = append(tasks, task)
	}
	return tasks
}

// modelFileParked reports whether task is still the key's parked task.
func (n *NodeRuntime) modelFileParked(task proto.ModelFileTask) bool {
	current, ok := n.parkedModelFile(task.Key)
	return ok && sameModelFileTask(current, task)
}

// parkModelFile makes task the key's parked task in the storage queue or the
// writer queue. A retry (the task came from a queue snapshot) parks again only
// while it is still the key's parked task, so it cannot put an outdated task
// back over a newer one or over an eviction.
func (n *NodeRuntime) parkModelFile(task proto.ModelFileTask, writer, retry bool) bool {
	if n.storage == nil {
		return false
	}
	s := n.storage
	s.mu.Lock()
	defer s.mu.Unlock()
	if retry {
		if current, ok := s.parkedLocked(task.Key); !ok || !sameModelFileTask(current, task) {
			return false
		}
	}
	delete(s.waiting, task.Key)
	delete(s.writerWaiting, task.Key)
	if writer {
		s.writerWaiting[task.Key] = task
	} else {
		s.waiting[task.Key] = task
	}
	return true
}

func (n *NodeRuntime) parkStorageModelFile(task proto.ModelFileTask, retry bool) bool {
	return n.parkModelFile(task, false, retry)
}

func (n *NodeRuntime) parkWriterModelFile(task proto.ModelFileTask, retry bool) bool {
	return n.parkModelFile(task, true, retry)
}

var parkWriterModelFileForTask = (*NodeRuntime).parkWriterModelFile

// settleParkedModelFile removes the task's queue entry once the task has been
// dealt with (started, completed, failed or found running). A retry may only
// act on its own parked task: false means it was evicted or replaced meanwhile
// and the caller must do nothing. A fresh task is never refused; it removes only
// an entry with its own TransferID.
func (n *NodeRuntime) settleParkedModelFile(task proto.ModelFileTask, retry bool) bool {
	if n.storage == nil {
		return true
	}
	s := n.storage
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.parkedLocked(task.Key)
	if retry && (!ok || !sameModelFileTask(current, task)) {
		return false
	}
	if ok && current.TransferID == task.TransferID {
		delete(s.waiting, task.Key)
		delete(s.writerWaiting, task.Key)
	}
	return true
}

// dropWaiting forgets the task parked for key, so it does not run when its
// storage recovers after the model was deleted meanwhile.
func (n *NodeRuntime) dropWaiting(key string) {
	if n.storage == nil {
		return
	}
	n.storage.mu.Lock()
	delete(n.storage.waiting, key)
	delete(n.storage.writerWaiting, key)
	n.storage.mu.Unlock()
}

// dropParkedModelFile forgets task if it is still the key's parked task.
func (n *NodeRuntime) dropParkedModelFile(task proto.ModelFileTask) {
	if n.storage == nil {
		return
	}
	s := n.storage
	s.mu.Lock()
	if current, ok := s.parkedLocked(task.Key); ok && sameModelFileTask(current, task) {
		delete(s.waiting, task.Key)
		delete(s.writerWaiting, task.Key)
	}
	s.mu.Unlock()
}

// dropParkedTransfer forgets the task parked for key when it belongs to
// transferID. A stop for an earlier transfer arrives late and must not remove
// the task of the transfer that replaced it.
func (n *NodeRuntime) dropParkedTransfer(key, transferID string) {
	if n.storage == nil {
		return
	}
	s := n.storage
	s.mu.Lock()
	if current, ok := s.parkedLocked(key); ok && current.TransferID == transferID {
		delete(s.waiting, key)
		delete(s.writerWaiting, key)
	}
	s.mu.Unlock()
}

// retryParkedModelFile runs a parked task from a queue snapshot. It runs the
// latest task parked for the key, picking up a changed rate limit or address
// list, and gives up when nothing is parked for the key any more or the parked
// task belongs to another transfer.
func (n *NodeRuntime) retryParkedModelFile(ctx context.Context, snapshot proto.ModelFileTask) {
	latest, ok := n.parkedModelFile(snapshot.Key)
	if !ok || latest.TransferID != snapshot.TransferID {
		return
	}
	n.reconcileModelFileTask(ctx, latest, true)
}

// wakeModelWriters retries the tasks waiting for a path owner or a copy slot
// after a writer returned.
func (n *NodeRuntime) wakeModelWriters(ctx context.Context) {
	if n.storage == nil || ctx.Err() != nil {
		return
	}
	s := n.storage
	s.mu.RLock()
	waiting := make([]proto.ModelFileTask, 0, len(s.writerWaiting))
	for _, task := range s.writerWaiting {
		waiting = append(waiting, task)
	}
	s.mu.RUnlock()
	if len(waiting) == 0 {
		return
	}
	go func() {
		for _, task := range waiting {
			if ctx.Err() != nil {
				return
			}
			n.retryParkedModelFile(ctx, task)
		}
	}()
}
