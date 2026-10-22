package prepobs

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// CopyTTL is how long a copy observation stays fresh without a new frame.
	CopyTTL = 15 * time.Second
	// CopyLimit bounds the copies observed at once.
	CopyLimit = 8192
)

type copyEntry struct {
	node   uint
	status proto.ModelFileStatus
}

// Copies are the agents' latest same-node copy frames, keyed by model-file
// key. A frame is evidence of progress only: the durable model-file row still
// decides readiness, and Observe only returns a frame of the row's current
// intent. Disconnect work is proportional to the node's own copies.
type Copies struct {
	mu      sync.Mutex
	entries *expiring[string, copyEntry]
	byNode  map[uint]map[string]struct{}
}

func NewCopies() *Copies {
	c := &Copies{entries: newExpiring[string, copyEntry](CopyTTL, CopyLimit), byNode: map[uint]map[string]struct{}{}}
	c.entries.dropped = func(key string, entry copyEntry) { c.unindex(entry.node, key) }
	return c
}

// Record keeps a reporting node's frame. A frame without a preparation
// observation, or of any state but downloading, retires the key.
func (c *Copies) Record(node uint, status proto.ModelFileStatus, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries.remove(status.Key); ok {
		c.unindex(old.node, status.Key)
	}
	if status.Preparation == nil || status.State != proto.MFDownloading {
		return
	}
	observation := *status.Preparation
	status.Preparation = &observation
	if !c.entries.put(status.Key, copyEntry{node, status}, now) {
		return
	}
	if c.byNode[node] == nil {
		c.byNode[node] = map[string]struct{}{}
	}
	c.byNode[node][status.Key] = struct{}{}
}

// ForgetNode drops every frame a node reported. The hub calls it in the same
// critical section that replaces or removes the node's session, so a frame
// from the old connection cannot recreate them.
func (c *Copies) ForgetNode(node uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.byNode[node] {
		c.entries.remove(key)
	}
	delete(c.byNode, node)
}

// Observe returns the fresh frame of the intent a row now holds: the row must
// still be downloading under the same transfer ID.
func (c *Copies) Observe(key, state, transferID string, now time.Time) (proto.ModelFileStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries.get(key, now)
	if !ok || state != proto.MFDownloading || entry.status.TransferID != transferID {
		return proto.ModelFileStatus{}, false
	}
	status := entry.status
	observation := *status.Preparation
	status.Preparation = &observation
	return status, true
}

func (c *Copies) unindex(node uint, key string) {
	delete(c.byNode[node], key)
	if len(c.byNode[node]) == 0 {
		delete(c.byNode, node)
	}
}
