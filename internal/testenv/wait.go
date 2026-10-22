package testenv

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// harnessWait bounds the harness's own waits (a node registering, its
// inventory landing). They normally finish in milliseconds; the bound only
// has to outlast a starved scheduler, and a short one turned heavy host load
// into false failures (todo T23).
const harnessWait = 60 * time.Second

// waitFor is Eventually for the harness's own waits: a timeout says what was
// awaited and the state seen last, and is reported at the caller's line.
func (e *Env) waitFor(t testing.TB, what string, condition func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(harnessWait)
	for {
		ok, state := condition()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s; last seen: %s", what, harnessWait, state)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// nodeReady reports whether a harness node is online and its first status
// report has landed. A node goes online the moment its session registers, but
// its inventory (GPUs, host facts) travels in the status.node report that
// follows; a case that deploys right after the node comes online can meet a
// node with no usable GPU on a loaded host and be refused with
// DEPLOYMENT_NEVER_FITS (no_healthy_gpu). Registration does not record the
// host's CPU count, so a recorded one marks that report.
func (e *Env) nodeReady(name string) (bool, string) {
	row, state := e.nodeState(name)
	if row.UUID == "" || row.State != store.NodeStateOnline {
		return false, state
	}
	if row.CPUCores == 0 {
		return false, state + ", first status report not received"
	}
	return true, state
}

// nodeState reports a harness node's row state for waitFor.
func (e *Env) nodeState(name string) (store.Node, string) {
	e.nodesMu.Lock()
	node := e.nodes[name]
	e.nodesMu.Unlock()
	if node == nil {
		return store.Node{}, "no such harness node"
	}
	rows, err := e.Store().ListNodes()
	if err != nil {
		return store.Node{}, "list nodes: " + err.Error()
	}
	for _, row := range rows {
		if row.UUID == node.UUID {
			return row, "state " + string(row.State)
		}
	}
	return store.Node{}, "not registered yet"
}
