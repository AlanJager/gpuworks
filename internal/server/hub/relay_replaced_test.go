package hub

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// An agent that connects again leaves its earlier connection half open on
// the member that held it (its address moved to another entry host). The
// relay of the earlier connection must not take the node back: the two would
// replace each other's session for as long as the earlier one stays open.
func TestRelayDoesNotTakeBackANodeAnotherConnectionReplaced(t *testing.T) {
	h := newTestHub(t)
	uuid, token := newTestNode(t, h.store)
	var attached atomic.Int64
	attach := func(_ context.Context, lane, sessionID, remote string) (net.Conn, uint64, bool, error) {
		attached.Add(1)
		near, far := net.Pipe()
		go h.ServeRelayedConn(far, lane, sessionID, remote)
		return near, 1, true, nil
	}
	relay := NewAgentRelay(nil, attach, func() (uint64, bool) { return 1, true })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { relay.ServeWS(w, r, r.RemoteAddr) }))
	t.Cleanup(func() { relay.Close(); srv.Close() })

	earlier, first := dialCtrlLane(t, srv, token)
	later, second := dialCtrlLane(t, srv, token)
	if first.SessionID == "" || first.SessionID == second.SessionID {
		t.Fatalf("session identities %q and %q, want two different ones", first.SessionID, second.SessionID)
	}
	// The relay lets the agent read the refusal before it closes.
	for deadline := time.Now().Add(10 * time.Second); !earlier.IsClosed() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !earlier.IsClosed() {
		t.Fatal("the replaced connection stayed open: its relay keeps asking for the node")
	}
	// Both relays had time to trade the node many times over.
	time.Sleep(6 * relayAttachRetry)
	if later.IsClosed() {
		t.Fatal("the connection that replaced the earlier one was closed")
	}
	h.mu.RLock()
	current := h.sessions[uuid]
	h.mu.RUnlock()
	if current == nil || current.sessionID != second.SessionID {
		t.Fatalf("the node's session is %v, want the later connection's %q", current, second.SessionID)
	}
	// Two first attachments and the one the hub refused.
	if got := attached.Load(); got != 3 {
		t.Fatalf("relays attached %d times, want 3", got)
	}
}

// The same relayed connection may open its session again: after a change of
// serving member the new one knows no replacement, and one that restarted
// its sessions is asked again by every relay.
func TestRelayedSessionMayRegisterAgainUnderItsOwnIdentity(t *testing.T) {
	h := newTestHub(t)
	uuid, token := newTestNode(t, h.store)
	attach := func(_ context.Context, lane, sessionID, remote string) (net.Conn, uint64, bool, error) {
		near, far := net.Pipe()
		go h.ServeRelayedConn(far, lane, sessionID, remote)
		return near, 1, true, nil
	}
	relay := NewAgentRelay(nil, attach, func() (uint64, bool) { return 1, true })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { relay.ServeWS(w, r, r.RemoteAddr) }))
	t.Cleanup(func() { relay.Close(); srv.Close() })

	agent, reg := dialCtrlLane(t, srv, token)
	h.mu.RLock()
	before := h.sessions[uuid]
	h.mu.RUnlock()
	before.Close() // the serving member ends the session; the agent's connection is fine
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.RLock()
		now := h.sessions[uuid]
		h.mu.RUnlock()
		if now != nil && now != before {
			if now.sessionID != reg.SessionID {
				t.Fatalf("session identity %q after the relay registered again, want %q", now.sessionID, reg.SessionID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay never registered the agent again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if agent.IsClosed() {
		t.Fatal("the agent's connection was closed")
	}
}
