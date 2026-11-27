package hub

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// parkSignal makes h report every waiter about to block; the returned function
// returns once one has parked at site. A test then acts on a parked waiter, so
// it exercises the wake-up path rather than a lucky schedule. Set it before the
// waiter starts.
func parkSignal(t *testing.T, h *Hub) func(site string) {
	t.Helper()
	ch := make(chan string, 64)
	h.waits.parkedHook = func(site string) {
		select {
		case ch <- site:
		default:
		}
	}
	return func(site string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case got := <-ch:
				if got == site {
					return
				}
			case <-deadline:
				t.Fatalf("no waiter parked at %q", site)
			}
		}
	}
}

// registeredSession builds a session for uuid over a fresh yamux pair and
// registers it, as an agent's ctrl handshake would. The returned peer end is
// closed with the test.
func registeredSession(t *testing.T, h *Hub, uuid string) *Session {
	t.Helper()
	ysess, _ := yamuxPairData(t)
	ctrl, ctrlPeer := net.Pipe()
	t.Cleanup(func() { _ = ctrlPeer.Close() })
	sess := newSession(h, ysess, ctrl)
	sess.NodeUUID = uuid
	h.register(sess)
	t.Cleanup(func() {
		h.deregister(sess)
		sess.Close()
	})
	return sess
}

// newDetachedSession builds, without registering, a session that once had a
// data lane and lost it: a data-plane open on it waits in awaitDataLane
// instead of riding ctrl.
func newDetachedSession(t *testing.T, h *Hub, uuid string) *Session {
	t.Helper()
	ysess, _ := yamuxPairData(t)
	ctrl, ctrlPeer := net.Pipe()
	t.Cleanup(func() { _ = ctrlPeer.Close() })
	sess := newSession(h, ysess, ctrl)
	sess.NodeUUID = uuid
	data, _ := yamuxPairData(t)
	sess.attachData(data)
	_ = data.Close()
	waitState(t, sess, "detached")
	return sess
}

// detachedSession registers such a session, as an agent's ctrl handshake
// would.
func detachedSession(t *testing.T, h *Hub, uuid string) *Session {
	t.Helper()
	sess := newDetachedSession(t, h, uuid)
	h.register(sess)
	t.Cleanup(func() {
		h.deregister(sess)
		sess.Close()
	})
	return sess
}

// TestOpenTunnel_OpensOnReplacementSession: a tunnel waiting on a detached
// data lane of a session the agent replaces must open on the node's next
// session (which carries it on ctrl until its own data lane attaches) rather
// than failing with "ctrl session closed".
func TestOpenTunnel_OpensOnReplacementSession(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)

	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := h.OpenTunnel(context.Background(), uuid, "127.0.0.1:30000")
		done <- result{conn, err}
	}()
	// The waiter is parked in awaitDataLane, so the wake-up under test is the
	// replacement rather than a lucky race.
	parked(waitSiteDataLane)

	replacement := registeredSession(t, h, uuid)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("OpenTunnel after session replacement: %v", r.err)
		}
		_ = r.conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("OpenTunnel never opened on the replacement session")
	}
	if got := replacement.CtrlDataPlaneStreamCount(); got != 1 {
		t.Fatalf("replacement session ctrl data-plane streams = %d, want 1", got)
	}
	if got := old.CtrlDataPlaneStreamCount(); got != 0 {
		t.Fatalf("replaced session ctrl data-plane streams = %d, want 0", got)
	}
}

// TestOpenTunnel_WaitsForFirstSession: a request that arrives in the window
// between an agent's disconnect and its next registration (its node still seen
// moments ago) waits for that session and opens on it, instead of failing with
// "node not connected" at once.
func TestOpenTunnel_WaitsForFirstSession(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	if err := h.store.TouchNodesAt(map[string]time.Time{uuid: time.Now()}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		conn net.Conn
		err  error
	}
	parked := parkSignal(t, h)
	done := make(chan result, 1)
	go func() {
		conn, err := h.OpenTunnel(context.Background(), uuid, "127.0.0.1:30000")
		done <- result{conn, err}
	}()
	parked(waitSiteSession)
	replacement := registeredSession(t, h, uuid)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("OpenTunnel while the agent re-registers: %v", r.err)
		}
		_ = r.conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("OpenTunnel never opened on the node's next session")
	}
	if got := replacement.CtrlDataPlaneStreamCount(); got != 1 {
		t.Fatalf("new session ctrl data-plane streams = %d, want 1", got)
	}
}

// TestOpenTunnel_StaleNodeAnswersAtOnce: a node with no session that has not
// been seen recently is refused immediately, as before — the node-scoped wait
// exists for a reconnect, not for a node that is really down.
func TestOpenTunnel_StaleNodeAnswersAtOnce(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	old := time.Now().Add(-time.Minute)
	if err := h.store.TouchNodesAt(map[string]time.Time{uuid: old}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	conn, err := h.OpenTunnel(context.Background(), uuid, "127.0.0.1:30000")
	if conn != nil || err == nil {
		t.Fatalf("OpenTunnel for a stale node = (%v, %v), want an error", conn, err)
	}
	if !strings.Contains(err.Error(), "node not connected") {
		t.Fatalf("err = %v, want the unchanged node-not-connected answer", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stale node took %v to be refused, want at once", elapsed)
	}
}

// TestOpenTunnel_FreshNodeWaitsThenFailsTyped: a node seen moments ago (its
// agent reconnecting) is waited for, but nothing registers, so the request
// ends with the retryable data-lane error after the wait budget — bounded by
// it rather than hanging.
func TestOpenTunnel_FreshNodeWaitsThenFailsTyped(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	if err := h.store.TouchNodesAt(map[string]time.Time{uuid: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.waits.tunnelBudget = 150 * time.Millisecond

	start := time.Now()
	_, err := h.OpenTunnel(context.Background(), uuid, "127.0.0.1:30000")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("err = %v, want ErrDataLaneUnavailable", err)
	}
	if elapsed < h.waits.tunnelBudget {
		t.Fatalf("wait took %v, want at least the %v budget", elapsed, h.waits.tunnelBudget)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wait took %v, want bounded by the budget", elapsed)
	}

	// The caller's context still cuts the wait short.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = h.OpenTunnel(ctx, uuid, "127.0.0.1:30000")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ctx deadline took %v to be observed, want well under the budget", elapsed)
	}
}

// TestOpenTunnel_ReplacementThatDiesFailsTyped: the replacement session closes
// too and no further one comes, so there is nothing left to open on. The
// request waits for the node within the same budget and then ends with the
// typed data-lane error; it does not hang.
func TestOpenTunnel_ReplacementThatDiesFailsTyped(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	h.waits.tunnelBudget = 200 * time.Millisecond

	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	done := make(chan error, 1)
	go func() {
		_, err := h.OpenTunnel(context.Background(), uuid, "127.0.0.1:30000")
		done <- err
	}()
	parked(waitSiteDataLane)

	// A replacement that is already dead on arrival: registered, detached and
	// closed. Whichever order the waiter observes it in, it has nothing to open
	// on, so the answer is the typed error.
	replacement := newDetachedSession(t, h, uuid)
	h.register(replacement)
	replacement.Close()
	t.Cleanup(func() {
		h.deregister(replacement)
	})

	select {
	case err := <-done:
		if !errors.Is(err, ErrDataLaneUnavailable) {
			t.Fatalf("err = %v, want ErrDataLaneUnavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenTunnel hung after the replacement session died")
	}
	if old.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("the replaced session must never carry a data-plane stream")
	}
}

// TestDataLaneRoute_CtrlCloseReportsDistinctCause keeps the session-level
// semantics unchanged but makes the closed-session cause distinguishable: the
// hub uses it to look for the node's next session.
func TestDataLaneRoute_CtrlCloseReportsDistinctCause(t *testing.T) {
	h := newTestHub(t)
	sess := detachedSession(t, h, "distinct-cause-node")
	parked := parkSignal(t, h)

	done := make(chan error, 1)
	go func() {
		_, _, err := sess.dataLaneRoute(context.Background(), proto.StreamTunnel)
		done <- err
	}()
	parked(waitSiteDataLane)
	sess.markClosed()

	select {
	case err := <-done:
		if !errors.Is(err, ErrDataLaneUnavailable) || !errors.Is(err, errCtrlSessionClosed) {
			t.Fatalf("err = %v, want ErrDataLaneUnavailable wrapping errCtrlSessionClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl close never woke the waiter")
	}
}

// TestOpenTunnel_WaitsAcrossDeregisterThenRegister: the usual reconnect order
// is the agent's ctrl connection dying first — the hub deregisters its session
// and the table is empty for a while — and the new session registering later.
// A tunnel waiting on the old session's detached lane must ride that gap and
// open on the new session, not answer "node not connected" in between.
func TestOpenTunnel_WaitsAcrossDeregisterThenRegister(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	if err := h.store.TouchNodesAt(map[string]time.Time{uuid: time.Now()}); err != nil {
		t.Fatal(err)
	}
	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := h.OpenTunnel(context.Background(), uuid, "127.0.0.1:30000")
		done <- result{conn, err}
	}()
	parked(waitSiteDataLane)
	h.deregister(old)
	old.Close()
	// The waiter is now parked on the node, with no session in the table.
	parked(waitSiteSession)
	replacement := registeredSession(t, h, uuid)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("OpenTunnel across deregister then register: %v", r.err)
		}
		_ = r.conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("OpenTunnel never opened on the node's next session")
	}
	if got := replacement.CtrlDataPlaneStreamCount(); got != 1 {
		t.Fatalf("new session ctrl data-plane streams = %d, want 1", got)
	}
}
