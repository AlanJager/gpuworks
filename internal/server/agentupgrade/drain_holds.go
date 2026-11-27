package agentupgrade

import "sync"

// drainHolds is, per attempt, the gateway preferences its upgrade took on the
// node's instances (drain.go): what to release when the attempt leaves its
// active phases. Each release is the gateway's own token, so releasing one
// attempt's hold cannot undo another holder's. It lives only in memory.
//
// The runner's one goroutine takes and releases holds; the lock keeps that
// true for a settle path that runs from another one.
type drainHolds struct {
	mu     sync.Mutex
	byItem map[uint]map[uint]func()
	// requeued counts, per attempt still pending, its returns to the queue
	// from a drain (maxDrainRequeues).
	requeued map[uint]int
}

func (h *drainHolds) noteRequeue(itemID uint) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.requeued == nil {
		h.requeued = map[uint]int{}
	}
	h.requeued[itemID]++
}

func (h *drainHolds) requeues(itemID uint) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requeued[itemID]
}

// forgetRequeues drops the counts of attempts no longer pending.
func (h *drainHolds) forgetRequeues(pending map[uint]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.requeued {
		if !pending[id] {
			delete(h.requeued, id)
		}
	}
}

// take holds the attempt's preferences, one per deployment of set.
func (h *drainHolds) take(itemID uint, gw Gateway, set drainSet) {
	releases := make(map[uint]func(), len(set))
	for deploymentID, list := range set {
		releases[deploymentID] = gw.Avoid(deploymentID, list)
	}
	h.mu.Lock()
	if h.byItem == nil {
		h.byItem = map[uint]map[uint]func(){}
	}
	previous := h.byItem[itemID]
	h.byItem[itemID] = releases
	h.mu.Unlock()
	for _, release := range previous {
		release()
	}
}

// drop ends the attempt's preference for one deployment.
func (h *drainHolds) drop(itemID, deploymentID uint) {
	h.mu.Lock()
	release := h.byItem[itemID][deploymentID]
	delete(h.byItem[itemID], deploymentID)
	h.mu.Unlock()
	if release != nil {
		release()
	}
}

// release ends every preference the attempt holds. It is safe for an attempt
// that holds none.
func (h *drainHolds) release(itemID uint) {
	h.mu.Lock()
	releases := h.byItem[itemID]
	delete(h.byItem, itemID)
	h.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

// held reports whether the attempt holds a drain here, even one whose every
// deployment already left the wait.
func (h *drainHolds) held(itemID uint) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.byItem[itemID]
	return ok
}

// releaseInactive ends the holds of attempts not in active.
func (h *drainHolds) releaseInactive(active map[uint]bool) {
	h.mu.Lock()
	var ended []uint
	for id := range h.byItem {
		if !active[id] {
			ended = append(ended, id)
		}
	}
	h.mu.Unlock()
	for _, id := range ended {
		h.release(id)
	}
}

// count is how many attempts hold a drain, for tests.
func (h *drainHolds) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.byItem)
}
