package usagereport

import (
	"sync"
)

// Counter kinds: the facts that only exist as they happen, so the database
// cannot be asked for them afterwards.
const (
	// KindRoute counts API calls per route group (the first path segment
	// after /api/v1), never the path itself.
	KindRoute = "route"
	// KindAPIError counts product errors the API answered, per "CODE route".
	KindAPIError = "api_error"
	// KindPanic counts recovered panics per innermost gpuworks function.
	KindPanic = "panic"
	// KindProtocol counts agent-link failures per reason: a refused
	// handshake (proto_mismatch, bad_token, stale_session) or a data lane
	// that did not come back in time (data_lane_unavailable).
	KindProtocol = "protocol"
	// KindLogin counts management sign-ins, as "ok" or "failed".
	KindLogin = "login"
)

// maxKeysPerKind caps the keys one kind may hold, so a stream of distinct
// keys cannot grow the counters without bound; the rest count as "other".
const maxKeysPerKind = 256

type counterKey struct{ kind, name string }

// Counters accumulate in-process events between two reports. Inc is the only
// call on a hot path; it takes one short lock. The zero value is ready.
type Counters struct {
	mu     sync.Mutex
	counts map[counterKey]int
	keys   map[string]int
}

// Inc adds one to name within kind. A nil receiver ignores the call, so code
// can count unconditionally whether or not the program is wired in.
func (c *Counters) Inc(kind, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addLocked(kind, name, 1)
}

func (c *Counters) addLocked(kind, name string, n int) {
	if c.counts == nil {
		c.counts = map[counterKey]int{}
		c.keys = map[string]int{}
	}
	key := counterKey{kind, name}
	if _, ok := c.counts[key]; !ok {
		if c.keys[kind] >= maxKeysPerKind {
			key.name = Other
		} else {
			c.keys[kind]++
		}
	}
	c.counts[key] += n
}

// Drain returns what was counted per kind since the last Drain and starts
// over. A report that could not be sent gives its counts back with Restore,
// so they ride on the next one instead of being lost or queued.
func (c *Counters) Drain() map[string][]Count {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	counts := c.counts
	c.counts, c.keys = nil, nil
	c.mu.Unlock()
	out := map[string][]Count{}
	for k, n := range counts {
		out[k.kind] = append(out[k.kind], Count{Name: k.name, N: n})
	}
	for _, list := range out {
		sortCounts(list)
	}
	return out
}

// Peek returns what Drain would, without starting over: the console's
// preview shows the next report without taking anything from it.
func (c *Counters) Peek() map[string][]Count {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	out := map[string][]Count{}
	for k, n := range c.counts {
		out[k.kind] = append(out[k.kind], Count{Name: k.name, N: n})
	}
	c.mu.Unlock()
	for _, list := range out {
		sortCounts(list)
	}
	return out
}

// Restore adds drained counts back.
func (c *Counters) Restore(drained map[string][]Count) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for kind, list := range drained {
		for _, x := range list {
			c.addLocked(kind, x.Name, x.N)
		}
	}
}
