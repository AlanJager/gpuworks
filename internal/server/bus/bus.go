// Package bus is a tiny in-process event bus. Publishers (store methods, after
// commit) fan out to subscribers over per-subscriber bounded channels. When a
// subscriber's buffer is full, events for the same (topic,key) are coalesced
// rather than blocking the publisher — a slow SSE client must never stall a DB
// write. Dropped-vs-coalesced counts are tracked for observability.
package bus

import (
	"sync"
	"sync/atomic"
)

// ReasonProgress marks a change that only moved an object's progress counters
// forward: no state, phase, or presence changed. Subscribers that reconcile on
// state (the controller) skip it; subscribers that render it (SSE clients) do
// not. It is the difference between a redeploy storm re-planning every
// deployment on each downloaded megabyte and not.
const ReasonProgress = "progress"

// Event is a change notification. Payload is intentionally absent: subscribers
// re-read current state from the store, which keeps events idempotent and small.
type Event struct {
	Topic  string // e.g. "node"
	Key    string // e.g. node UUID
	Reason string // human hint: "registered", "status", "state:offline", "progress"
}

type subscriber struct {
	topics map[string]bool
	ch     chan Event
	// pending holds the latest event per coalesce-key while ch is full.
	mu       sync.Mutex
	pending  map[string]Event
	order    []string
	coalesce atomic.Int64
}

// Bus routes events to subscribers.
type Bus struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
	// generations counts each topic's non-progress events (topic ->
	// *atomic.Uint64), for a reader that only asks whether anything changed
	// since it last looked and has no use for a subscription's events.
	generations sync.Map
}

// New creates an empty bus.
func New() *Bus {
	return &Bus{subs: make(map[*subscriber]struct{})}
}

// Subscription is the consumer handle returned by Subscribe.
type Subscription struct {
	bus *Bus
	sub *subscriber
}

// Subscribe registers interest in one or more topics (empty = all topics).
// buffer is the per-subscriber channel depth.
func (b *Bus) Subscribe(buffer int, topics ...string) *Subscription {
	if buffer <= 0 {
		buffer = 64
	}
	s := &subscriber{
		topics:  make(map[string]bool, len(topics)),
		ch:      make(chan Event, buffer),
		pending: make(map[string]Event),
	}
	for _, t := range topics {
		s.topics[t] = true
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return &Subscription{bus: b, sub: s}
}

// C returns the receive channel. The pump drains coalesced events onto it.
func (sub *Subscription) C() <-chan Event { return sub.sub.ch }

// Close removes the subscription.
func (sub *Subscription) Close() {
	sub.bus.mu.Lock()
	delete(sub.bus.subs, sub.sub)
	sub.bus.mu.Unlock()
}

// CoalescedCount reports how many events were merged due to backpressure.
func (sub *Subscription) CoalescedCount() int64 { return sub.sub.coalesce.Load() }

// PublishChange is a convenience for the common (topic, key, reason) publish.
func (b *Bus) PublishChange(topic, key, reason string) {
	b.Publish(Event{Topic: topic, Key: key, Reason: reason})
}

// Generation is how many non-progress events topic has seen. A cache derived
// from a topic's objects keeps the generation it was built at and is stale
// once the bus moves past it; reading it costs one atomic load and never a
// subscription that would have to be drained.
func (b *Bus) Generation(topic string) uint64 {
	if counter, ok := b.generations.Load(topic); ok {
		return counter.(*atomic.Uint64).Load()
	}
	return 0
}

// Publish delivers an event to all interested subscribers. Never blocks.
func (b *Bus) Publish(e Event) {
	if e.Reason != ReasonProgress {
		counter, ok := b.generations.Load(e.Topic)
		if !ok {
			counter, _ = b.generations.LoadOrStore(e.Topic, new(atomic.Uint64))
		}
		counter.(*atomic.Uint64).Add(1)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if len(s.topics) > 0 && !s.topics[e.Topic] {
			continue
		}
		s.deliver(e)
	}
}

func (s *subscriber) deliver(e Event) {
	key := e.Topic + "\x00" + e.Key
	select {
	case s.ch <- e:
		return
	default:
		// Buffer full: coalesce by key. Newest wins, except that progress never
		// displaces a state change; a pump goroutine flushes.
		s.mu.Lock()
		held, exists := s.pending[key]
		if exists {
			s.coalesce.Add(1)
		} else {
			s.order = append(s.order, key)
		}
		// A progress notice must not replace a held state change: subscribers
		// that skip progress would then never learn the state moved.
		if !exists || e.Reason != ReasonProgress || held.Reason == ReasonProgress {
			s.pending[key] = e
		}
		s.mu.Unlock()
		s.tryFlush()
	}
}

// tryFlush moves pending events onto the channel opportunistically.
func (s *subscriber) tryFlush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.order) > 0 {
		key := s.order[0]
		e := s.pending[key]
		select {
		case s.ch <- e:
			delete(s.pending, key)
			s.order = s.order[1:]
		default:
			return
		}
	}
}
