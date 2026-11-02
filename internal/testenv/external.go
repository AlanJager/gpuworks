package testenv

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// CreatePendingNode creates credentials for a real external agent. The caller
// is responsible for installing that agent and may then use WaitNodeOnlineUUID.
func (e *Env) CreatePendingNode(t testing.TB, name string) (uuid, token string) {
	t.Helper()
	node, token, err := e.Store().CreateNodePending(name)
	if err != nil {
		t.Fatalf("create pending node %q: %v", name, err)
	}
	return node.UUID, token
}

// NodeOnlineUUID reports whether the node is online right now. A server_dial
// node can come online without the caller's own dial: the hub redials every
// registered server_dial address on its reconnect interval, and an agent serves
// one server connection at a time.
func (e *Env) NodeOnlineUUID(uuid string) bool {
	nodes, err := e.Store().ListNodes()
	if err != nil {
		return false
	}
	for _, node := range nodes {
		if node.UUID == uuid {
			return node.State == store.NodeStateOnline
		}
	}
	return false
}

func (e *Env) WaitNodeOnlineUUID(t testing.TB, uuid string) {
	e.WaitNodeStateUUID(t, uuid, store.NodeStateOnline, 3*time.Minute)
}

func (e *Env) WaitNodeStateUUID(t testing.TB, uuid, state string, timeout time.Duration) {
	t.Helper()
	e.Eventually(t, timeout, func() bool {
		nodes, err := e.Store().ListNodes()
		if err != nil {
			return false
		}
		for _, node := range nodes {
			if node.UUID == uuid {
				return node.State == state
			}
		}
		return false
	})
}
