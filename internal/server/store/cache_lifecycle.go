package store

import "sync"

// cacheLifecycleGuard protects the interval in which a launch has read a
// ready cache file but has not persisted its instance references yet. Its
// mutex protects only reservations; it is never held during database or RPC
// work. Entries disappear when their last reservation ends.
type cacheLifecycleGuard struct {
	mu    sync.Mutex
	nodes map[uint]int // positive: readers; -1: eviction
}

// TryPinNodeCaches reserves all nodes for a launch, or none if an eviction
// owns any of them. Hold the returned release through the transaction that
// persists every instance's model-file references. Unrelated nodes remain
// available, and a conflict never waits for a node RPC.
func (s *Store) TryPinNodeCaches(nodeIDs []uint) (release func(), ok bool) {
	ids := make(map[uint]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		ids[id] = struct{}{}
	}
	g := &s.cacheLifecycle
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range ids {
		if g.nodes[id] < 0 {
			return nil, false
		}
	}
	if g.nodes == nil {
		g.nodes = make(map[uint]int)
	}
	for id := range ids {
		g.nodes[id]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			for id := range ids {
				g.nodes[id]--
				if g.nodes[id] == 0 {
					delete(g.nodes, id)
				}
			}
		})
	}, true
}

// TryEvictNodeCache exclusively reserves a node for cache deletion. Acquire
// it before the final eligibility check and release only after both the
// remote deletion and the database update. A busy node must be retried;
// waiting here could stall the deployment controller behind a slow RPC.
func (s *Store) TryEvictNodeCache(nodeID uint) (release func(), ok bool) {
	g := &s.cacheLifecycle
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.nodes[nodeID] != 0 {
		return nil, false
	}
	if g.nodes == nil {
		g.nodes = make(map[uint]int)
	}
	g.nodes[nodeID] = -1
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.nodes, nodeID)
		})
	}, true
}
