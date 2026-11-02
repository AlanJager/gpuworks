package connbudget

import "sync"

// Hosts holds one Pool per upstream address (a registry host, a proxy), made
// on first use with a shared ceiling: connection limits and "come back later"
// answers belong to an address, not to a node as a whole.
type Hosts struct {
	mu      sync.Mutex
	ceiling int
	pools   map[string]*Pool
}

// NewHosts returns an empty set whose pools admit ceiling requests each; a
// ceiling below one means DefaultCeiling.
func NewHosts(ceiling int) *Hosts {
	if ceiling < 1 {
		ceiling = DefaultCeiling
	}
	return &Hosts{ceiling: ceiling, pools: map[string]*Pool{}}
}

// For returns the pool of the upstream named key.
func (h *Hosts) For(key string) *Pool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pools[key]
	if !ok {
		p = NewPool(h.ceiling)
		h.pools[key] = p
	}
	return p
}

// SetCeiling changes the ceiling of every pool, existing and future.
func (h *Hosts) SetCeiling(ceiling int) {
	if ceiling < 1 {
		ceiling = DefaultCeiling
	}
	h.mu.Lock()
	h.ceiling = ceiling
	pools := make([]*Pool, 0, len(h.pools))
	for _, p := range h.pools {
		pools = append(pools, p)
	}
	h.mu.Unlock()
	for _, p := range pools {
		p.SetCeiling(ceiling)
	}
}

// Ceiling is what a new pool gets.
func (h *Hosts) Ceiling() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ceiling
}
