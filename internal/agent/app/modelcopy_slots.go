package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A hung filesystem call retains its task slot until the worker actually
// returns. writerSlotAvailableLocked also caps blocked copy workers per node.
const maxConcurrentModelCopies = 4

type modelFileIntent struct {
	task       proto.ModelFileTask
	generation uint64
}

type modelFileIntentContextKey struct{}

func withModelFileIntent(ctx context.Context, intent modelFileIntent) context.Context {
	return context.WithValue(ctx, modelFileIntentContextKey{}, intent)
}

func modelFileIntentFromContext(ctx context.Context) (modelFileIntent, bool) {
	intent, ok := ctx.Value(modelFileIntentContextKey{}).(modelFileIntent)
	return intent, ok
}

func (n *NodeRuntime) reportModelFileWithIntent(ctx context.Context, intent modelFileIntent, st proto.ModelFileStatus) {
	n.reportModelFileContext(withModelFileIntent(ctx, intent), st)
}

func (n *NodeRuntime) beginModelFileIntent(task proto.ModelFileTask) modelFileIntent {
	intent := modelFileIntent{task: task}
	if n.storage == nil {
		return intent
	}
	// discard waits for an in-flight send before the new intent becomes visible;
	// later callbacks from the old generation are fenced at their actual send.
	n.progress.discard(modelFileProgressKey(task.Key))
	s := n.storage
	s.mu.Lock()
	s.nextGeneration++
	intent.generation = s.nextGeneration
	s.latestGeneration[task.Key] = intent.generation
	delete(s.waiting, task.Key)
	delete(s.waitingGeneration, task.Key)
	delete(s.writerWaiting, task.Key)
	s.mu.Unlock()
	return intent
}

func (n *NodeRuntime) modelFileIntentCurrent(intent modelFileIntent) bool {
	if n.storage == nil || intent.generation == 0 {
		return true
	}
	s := n.storage
	s.mu.RLock()
	current := s.latestGeneration[intent.task.Key] == intent.generation
	s.mu.RUnlock()
	return current
}

func (n *NodeRuntime) clearModelFileWaiting(intent modelFileIntent) {
	if n.storage == nil || intent.generation == 0 {
		return
	}
	s := n.storage
	key := intent.task.Key
	s.mu.Lock()
	if s.waitingGeneration[key] == intent.generation {
		delete(s.waiting, key)
		delete(s.waitingGeneration, key)
	}
	if waiting := s.writerWaiting[key]; waiting.generation == intent.generation {
		delete(s.writerWaiting, key)
	}
	s.mu.Unlock()
}

func (n *NodeRuntime) parkStorageModelFile(intent modelFileIntent) bool {
	if n.storage == nil {
		return false
	}
	s := n.storage
	key := intent.task.Key
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent.generation != 0 && s.latestGeneration[key] != intent.generation {
		return false
	}
	s.waiting[key] = intent.task
	s.waitingGeneration[key] = intent.generation
	if waiting := s.writerWaiting[key]; waiting.generation == intent.generation {
		delete(s.writerWaiting, key)
	}
	return true
}

func (n *NodeRuntime) parkWriterModelFile(intent modelFileIntent) bool {
	if n.storage == nil {
		return false
	}
	s := n.storage
	key := intent.task.Key
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent.generation != 0 && s.latestGeneration[key] != intent.generation {
		return false
	}
	s.writerWaiting[key] = intent
	if s.waitingGeneration[key] == intent.generation {
		delete(s.waiting, key)
		delete(s.waitingGeneration, key)
	}
	return true
}

func (n *NodeRuntime) reconcileParkedModelFile(ctx context.Context, intent modelFileIntent) {
	if intent.generation == 0 {
		n.reconcileModelFile(ctx, intent.task)
		return
	}
	n.reconcileModelFileIntent(ctx, intent)
}

func (n *NodeRuntime) wakeModelWriters(ctx context.Context) {
	if n.storage == nil || ctx.Err() != nil {
		return
	}
	s := n.storage
	s.mu.Lock()
	waiting := make([]modelFileIntent, 0, len(s.writerWaiting))
	for key, intent := range s.writerWaiting {
		if intent.generation != 0 && s.latestGeneration[key] != intent.generation {
			delete(s.writerWaiting, key)
			continue
		}
		waiting = append(waiting, intent)
	}
	s.mu.Unlock()
	if len(waiting) == 0 {
		return
	}
	go func() {
		for _, intent := range waiting {
			if ctx.Err() != nil {
				return
			}
			n.reconcileParkedModelFile(ctx, intent)
		}
	}()
}

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

func (n *NodeRuntime) copyTaskRunning(task proto.ModelFileTask) bool {
	return n.copyTaskRunningIntent(n.beginModelFileIntent(task))
}

func (n *NodeRuntime) copyTaskRunningIntent(intent modelFileIntent) bool {
	task := intent.task
	n.dlMu.Lock()
	if !n.modelFileIntentCurrent(intent) {
		n.dlMu.Unlock()
		return true
	}
	if stopped, ok := n.stoppedCopies[task.Key]; ok {
		if stopped == task.TransferID {
			n.dlMu.Unlock()
			n.clearModelFileWaiting(intent)
			return true
		}
		delete(n.stoppedCopies, task.Key)
	}
	active := n.downloading[task.Key]
	if active != nil && active.copy && active.transferID != task.TransferID {
		active.cancel()
		n.dlMu.Unlock()
		n.parkWriterModelFile(intent)
		return true
	}
	if active != nil && !active.copy && task.Acquisition == proto.ModelAcquireCopy {
		n.dlMu.Unlock()
		n.parkWriterModelFile(intent)
		return true
	}
	n.dlMu.Unlock()
	if active != nil {
		n.clearModelFileWaiting(intent)
	}
	return active != nil
}
