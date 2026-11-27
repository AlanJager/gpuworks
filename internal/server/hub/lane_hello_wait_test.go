package hub

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ctrlSessionWithID registers a session that presents sessionID, as a ctrl
// handshake would have issued it, without a data lane.
func ctrlSessionWithID(t *testing.T, h *Hub, uuid, sessionID string) *Session {
	t.Helper()
	ysess, _ := yamuxPairData(t)
	ctrl, ctrlPeer := net.Pipe()
	t.Cleanup(func() { _ = ctrlPeer.Close() })
	sess := newSession(h, ysess, ctrl)
	sess.NodeUUID = uuid
	sess.sessionID = sessionID
	return sess
}

// TestDataLaneHello_WaitsForItsSessionToRegister: a data lane's hello can
// reach this member just before the relayed ctrl session that issued its id
// (a leader change). It must attach once that session registers, within the
// hello wait, instead of being rejected as stale and sent into the agent's
// backoff.
func TestDataLaneHello_WaitsForItsSessionToRegister(t *testing.T) {
	h := newTestHub(t)
	uuid, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)

	const sessionID = "waiting-for-its-ctrl-session"
	sess := ctrlSessionWithID(t, h, uuid, sessionID)
	parked := parkSignal(t, h)
	registered := make(chan struct{})
	go func() {
		defer close(registered)
		parked(waitSiteHello)
		h.register(sess)
	}()
	t.Cleanup(func() { <-registered })
	t.Cleanup(func() {
		h.deregister(sess)
		sess.Close()
	})

	env := dialDataRaw(t, srv, proto.DataHello{NodeToken: token, SessionID: sessionID})
	if env.T != proto.TDataAck {
		var rej proto.Reject
		_ = json.Unmarshal(env.D, &rej)
		t.Fatalf("data hello reply = %q (reason %q), want an ack", env.T, rej.Reason)
	}
	waitState(t, sess, "attached")
	if got := h.DataLaneState(uuid); got != "attached" {
		t.Fatalf("data lane state = %q, want attached", got)
	}
}

// TestDataLaneHello_UnregisteredSessionRejectedAfterWait: when no session with
// the presented id ever registers, the hello is still rejected as stale, after
// the bounded wait rather than at once or never.
func TestDataLaneHello_UnregisteredSessionRejectedAfterWait(t *testing.T) {
	h := newTestHub(t)
	h.waits.helloBudget = 150 * time.Millisecond
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)

	start := time.Now()
	env := dialDataRaw(t, srv, proto.DataHello{NodeToken: token, SessionID: "never-registered"})
	elapsed := time.Since(start)

	if env.T != proto.TReject {
		t.Fatalf("data hello reply = %q, want a reject", env.T)
	}
	var rej proto.Reject
	if err := json.Unmarshal(env.D, &rej); err != nil || rej.Reason != proto.RejectStaleSession {
		t.Fatalf("reject = %+v (%v), want stale_session", rej, err)
	}
	if elapsed < h.waits.helloBudget {
		t.Fatalf("reject after %v, want the %v wait to have elapsed", elapsed, h.waits.helloBudget)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("reject after %v, want bounded by the hello wait", elapsed)
	}
}

// TestDataLaneHello_MismatchedSessionRejectedAtOnce: a session registered
// under another id proves the presented one is gone, so the hello is rejected
// without waiting it out.
func TestDataLaneHello_MismatchedSessionRejectedAtOnce(t *testing.T) {
	h := newTestHub(t)
	h.waits.helloBudget = 5 * time.Second // a wait would show up in the elapsed time
	uuid, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)

	sess := ctrlSessionWithID(t, h, uuid, "the-current-session")
	h.register(sess)
	t.Cleanup(func() {
		h.deregister(sess)
		sess.Close()
	})

	start := time.Now()
	env := dialDataRaw(t, srv, proto.DataHello{NodeToken: token, SessionID: "an-older-session"})
	if env.T != proto.TReject {
		t.Fatalf("data hello reply = %q, want a reject", env.T)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("mismatched session rejected after %v, want at once", elapsed)
	}
}

// TestDataLaneHello_AnotherSessionRegisteredMidWaitRejectsAtOnce: the hello
// is already waiting (no session for the node yet) when a session with a
// different id registers. That proves the presented id is gone, so the wait
// ends at once with stale_session rather than running out its budget.
func TestDataLaneHello_AnotherSessionRegisteredMidWaitRejectsAtOnce(t *testing.T) {
	h := newTestHub(t)
	h.waits.helloBudget = 5 * time.Second // a wait would show up in the elapsed time
	uuid, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)

	other := ctrlSessionWithID(t, h, uuid, "the-session-that-registered")
	parked := parkSignal(t, h)
	registered := make(chan struct{})
	go func() {
		defer close(registered)
		parked(waitSiteHello)
		h.register(other)
	}()
	t.Cleanup(func() {
		<-registered
		h.deregister(other)
		other.Close()
	})

	start := time.Now()
	env := dialDataRaw(t, srv, proto.DataHello{NodeToken: token, SessionID: "the-session-it-was-issued"})
	elapsed := time.Since(start)
	if env.T != proto.TReject {
		t.Fatalf("data hello reply = %q, want a reject", env.T)
	}
	var rej proto.Reject
	if err := json.Unmarshal(env.D, &rej); err != nil || rej.Reason != proto.RejectStaleSession {
		t.Fatalf("reject = %+v (%v), want stale_session", rej, err)
	}
	if elapsed > time.Second {
		t.Fatalf("rejected after %v, want at once when another session registered mid-wait", elapsed)
	}
}
