package store

import "testing"

func TestCacheLifecyclePinsAllRanksAndReleasesReservations(t *testing.T) {
	s := &Store{}
	release, ok := s.TryPinNodeCaches([]uint{1, 2, 1})
	if !ok {
		t.Fatal("initial pin refused")
	}
	second, ok := s.TryPinNodeCaches([]uint{2})
	if !ok {
		t.Fatal("concurrent reader refused")
	}
	for _, id := range []uint{1, 2} {
		if unlock, ok := s.TryEvictNodeCache(id); ok {
			unlock()
			t.Fatalf("pinned node %d allowed eviction", id)
		}
	}
	unrelated, ok := s.TryEvictNodeCache(3)
	if !ok {
		t.Fatal("unrelated node was blocked")
	}
	unrelated()
	release()
	release() // Duplicate cleanup must not remove another launch's pin.
	if unlock, ok := s.TryEvictNodeCache(2); ok {
		unlock()
		t.Fatal("another launch's pin was released")
	}
	second()
	for _, id := range []uint{1, 2} {
		unlock, ok := s.TryEvictNodeCache(id)
		if !ok {
			t.Fatalf("node %d remained pinned", id)
		}
		unlock()
	}
	if len(s.cacheLifecycle.nodes) != 0 {
		t.Fatalf("unused reservations retained: %+v", s.cacheLifecycle.nodes)
	}
}

func TestCacheLifecycleEvictionRefusesGangWithoutPartialPins(t *testing.T) {
	s := &Store{}
	release, ok := s.TryEvictNodeCache(2)
	if !ok {
		t.Fatal("initial eviction refused")
	}
	if unlock, ok := s.TryPinNodeCaches([]uint{1, 2, 3}); ok {
		unlock()
		t.Fatal("launch pinned a node being evicted")
	}
	if unlock, ok := s.TryEvictNodeCache(2); ok {
		unlock()
		t.Fatal("simultaneous eviction accepted")
	}
	for _, id := range []uint{1, 3} {
		unlock, ok := s.TryEvictNodeCache(id)
		if !ok {
			t.Fatalf("failed gang left node %d pinned", id)
		}
		unlock()
	}
	release()
	unlock, ok := s.TryPinNodeCaches([]uint{1, 2, 3})
	if !ok {
		t.Fatal("launch remained blocked after eviction")
	}
	unlock()
}
