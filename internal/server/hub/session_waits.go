package hub

import (
	"sync"
	"time"
)

// sessionWaits wakes whoever waits for one node's session — a data-plane open
// that lost its session or found none (Hub.openTunnel), a data lane's hello
// that arrived before its ctrl session (Hub.awaitSessionID) — when that node's
// entry in the session table changes. It is kept per node: in a fleet's
// reconnect storm each waiter wakes only for its own node, and a register or
// deregister of a node nobody waits for costs one map lookup under w.mu.
//
// register and deregister call notify while holding h.mu, after they changed
// the table; a waiter takes the channel from next before it reads the table.
// So a change either is already in the table the waiter reads, or closes the
// channel it then blocks on: no wake-up is lost. w.mu is never held while
// taking h.mu.
//
// The zero value is ready to use.
type sessionWaits struct {
	mu    sync.Mutex
	nodes map[string]*nodeWake

	// tunnelBudget and helloBudget shorten the waits for tests; zero uses
	// dataLaneReattachWait and dataHelloSessionWait.
	tunnelBudget time.Duration
	helloBudget  time.Duration
	// parkedHook, set only by tests, is told each time a waiter is about to
	// block, so a test can act once it is parked.
	parkedHook func(site string)
}

// nodeWake is one node's wake-up channel, closed and replaced on each change,
// and how many waiters hold it; the entry goes when the last one leaves.
type nodeWake struct {
	changed chan struct{}
	waiters int
}

// watch counts a waiter for uuid. next returns the channel the next change of
// uuid's session closes; take it before reading the table. Call stop once
// done waiting.
func (w *sessionWaits) watch(uuid string) (next func() <-chan struct{}, stop func()) {
	w.mu.Lock()
	if w.nodes == nil {
		w.nodes = make(map[string]*nodeWake)
	}
	e := w.nodes[uuid]
	if e == nil {
		e = &nodeWake{changed: make(chan struct{})}
		w.nodes[uuid] = e
	}
	e.waiters++
	w.mu.Unlock()
	next = func() <-chan struct{} {
		w.mu.Lock()
		defer w.mu.Unlock()
		return e.changed
	}
	stop = func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if e.waiters--; e.waiters == 0 && w.nodes[uuid] == e {
			delete(w.nodes, uuid)
		}
	}
	return next, stop
}

// notify wakes uuid's waiters. Callers hold h.mu for writing and have already
// changed uuid's entry in the session table.
func (w *sessionWaits) notify(uuid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.nodes[uuid]; e != nil {
		close(e.changed)
		e.changed = make(chan struct{})
	}
}

// watched reports how many nodes have a waiter, for tests.
func (w *sessionWaits) watched() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.nodes)
}
