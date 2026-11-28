package hub

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

type budgetResult struct {
	conn    net.Conn
	err     error
	elapsed time.Duration
}

// openTunnelAsync starts an OpenTunnel and returns its eventual result.
func openTunnelAsync(ctx context.Context, h *Hub, uuid string) <-chan budgetResult {
	done := make(chan budgetResult, 1)
	go func() {
		start := time.Now()
		conn, err := h.OpenTunnel(ctx, uuid, "127.0.0.1:30000")
		done <- budgetResult{conn: conn, err: err, elapsed: time.Since(start)}
	}()
	return done
}

func awaitBudgetResult(t *testing.T, done <-chan budgetResult) budgetResult {
	t.Helper()
	select {
	case r := <-done:
		if r.conn != nil {
			t.Cleanup(func() { _ = r.conn.Close() })
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("OpenTunnel did not return within its budget")
		return budgetResult{}
	}
}

// requireBudgetSpent asserts the answer to a spent budget: the typed lane
// error the gateway turns into a retryable 503, after the budget and well
// before a second one (or the 5s an inner wait of its own would take).
func requireBudgetSpent(t *testing.T, r budgetResult, budget time.Duration, message string) {
	t.Helper()
	if !errors.Is(r.err, ErrDataLaneUnavailable) {
		t.Fatalf("err = %v, want ErrDataLaneUnavailable", r.err)
	}
	if errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, must not read as the caller's deadline", r.err)
	}
	if !strings.Contains(r.err.Error(), message) {
		t.Fatalf("err = %q, want it to say %q", r.err, message)
	}
	if r.elapsed < budget {
		t.Fatalf("answered after %v, want the whole %v budget waited", r.elapsed, budget)
	}
	if r.elapsed > 2*budget+time.Second {
		t.Fatalf("answered after %v, want one %v budget, not more", r.elapsed, budget)
	}
}

// dualLaneSession builds, without registering, a session of an agent that
// opens a data lane (it was issued a session id) whose lane has not attached.
func dualLaneSession(t *testing.T, h *Hub, uuid string) *Session {
	t.Helper()
	ysess, _ := yamuxPairData(t)
	ctrl, ctrlPeer := net.Pipe()
	t.Cleanup(func() { _ = ctrlPeer.Close() })
	sess := newSession(h, ysess, ctrl)
	sess.NodeUUID = uuid
	sess.sessionID = "dual-lane-session"
	return sess
}

func registerForTest(t *testing.T, h *Hub, s *Session) {
	t.Helper()
	h.register(s)
	t.Cleanup(func() {
		h.deregister(s)
		s.Close()
	})
}

// TestOpenTunnel_NoReplacementWithinBudget: the agent's ctrl connection drops
// while a tunnel waits on its detached data lane, and no new session registers
// within the budget. The answer is the typed lane error after one budget, so
// the gateway answers a retryable 503 instead of waiting again.
func TestOpenTunnel_NoReplacementWithinBudget(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	h.waits.tunnelBudget = 200 * time.Millisecond
	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	done := openTunnelAsync(context.Background(), h, uuid)
	parked(waitSiteDataLane)
	h.deregister(old)
	old.Close()
	parked(waitSiteSession)

	requireBudgetSpent(t, awaitBudgetResult(t, done), h.waits.tunnelBudget, "has no live agent session")
}

// TestOpenTunnel_FirstOpenSharesTheBudget: the session found at once has a
// detached lane. Its wait is the open's budget, not a fresh 5s of its own, so
// the open answers after one budget.
func TestOpenTunnel_FirstOpenSharesTheBudget(t *testing.T) {
	h := newTestHub(t)
	counters := &usagereport.Counters{}
	h.SetUsageCounters(counters)
	uuid, _ := newTestNode(t, h.store)
	h.waits.tunnelBudget = 200 * time.Millisecond
	_ = detachedSession(t, h, uuid)

	r := awaitBudgetResult(t, openTunnelAsync(context.Background(), h, uuid))
	requireBudgetSpent(t, r, h.waits.tunnelBudget, "data lane did not reattach")
	if got := protocolCount(counters, "data_lane_unavailable"); got != 1 {
		t.Fatalf("data_lane_unavailable counted %d times, want 1", got)
	}
}

// TestOpenTunnel_DetachedReplacementSpendsBudget: the node's next session
// registers, but its own data lane attached and dropped again (a lane that
// keeps flapping), so the open waits on it until the shared budget runs out.
// That is the hub's wait ending, not the caller's deadline: the typed lane
// error, counted once, not context.DeadlineExceeded (a 502 at the gateway).
func TestOpenTunnel_DetachedReplacementSpendsBudget(t *testing.T) {
	h := newTestHub(t)
	counters := &usagereport.Counters{}
	h.SetUsageCounters(counters)
	uuid, _ := newTestNode(t, h.store)
	h.waits.tunnelBudget = 200 * time.Millisecond
	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	done := openTunnelAsync(context.Background(), h, uuid)
	parked(waitSiteDataLane)
	replacement := newDetachedSession(t, h, uuid)
	registerForTest(t, h, replacement) // closes old
	parked(waitSiteDataLane)

	requireBudgetSpent(t, awaitBudgetResult(t, done), h.waits.tunnelBudget, "data lane did not reattach")
	if got := protocolCount(counters, "data_lane_unavailable"); got != 1 {
		t.Fatalf("data_lane_unavailable counted %d times, want 1", got)
	}
	if old.CtrlDataPlaneStreamCount() != 0 || replacement.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("a detached session must never carry a data-plane stream on ctrl")
	}
}

// TestOpenTunnel_FollowsAnAgentThatDropsTwice: the replacement session closes
// too before its lane comes back. The open follows the node to its third
// session within the same budget instead of failing (and instead of the
// gateway resending the request to start a second budget).
func TestOpenTunnel_FollowsAnAgentThatDropsTwice(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	_ = detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	done := openTunnelAsync(context.Background(), h, uuid)
	parked(waitSiteDataLane)
	second := newDetachedSession(t, h, uuid)
	registerForTest(t, h, second)
	parked(waitSiteDataLane)
	third := registeredSession(t, h, uuid) // closes second; single-lane, so ctrl

	r := awaitBudgetResult(t, done)
	if r.err != nil {
		t.Fatalf("OpenTunnel across two reconnects: %v", r.err)
	}
	if got := third.CtrlDataPlaneStreamCount(); got != 1 {
		t.Fatalf("third session ctrl data-plane streams = %d, want 1", got)
	}
	if second.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("a detached session must never carry a data-plane stream on ctrl")
	}
}

// TestOpenTunnel_BacklogWaitsForTheNewSessionsDataLane: requests that waited
// through a dual-lane agent's reconnect do not move onto its new ctrl
// connection; they wait for its data lane in the rest of the budget and open
// there.
func TestOpenTunnel_BacklogWaitsForTheNewSessionsDataLane(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	done := openTunnelAsync(context.Background(), h, uuid)
	parked(waitSiteDataLane)
	next := dualLaneSession(t, h, uuid)
	registerForTest(t, h, next) // closes old
	// Parked again: on the new session's lane, not riding its ctrl.
	parked(waitSiteDataLane)
	data, _ := yamuxPairData(t)
	next.attachData(data)

	r := awaitBudgetResult(t, done)
	if r.err != nil {
		t.Fatalf("OpenTunnel once the new session's lane attached: %v", r.err)
	}
	if _, onLane := r.conn.(*laneStream); !onLane {
		t.Fatalf("conn = %T, want it opened on the data lane", r.conn)
	}
	if next.CtrlDataPlaneStreamCount() != 0 || old.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("the backlog of a reconnect must not ride the new ctrl connection")
	}
}

// TestOpenTunnel_BacklogLaneThatNeverComesIsTyped: the dual-lane agent's new
// session never attaches its lane within the budget. The backlog gets the
// typed lane error, counted, and still never rides ctrl.
func TestOpenTunnel_BacklogLaneThatNeverComesIsTyped(t *testing.T) {
	h := newTestHub(t)
	counters := &usagereport.Counters{}
	h.SetUsageCounters(counters)
	uuid, _ := newTestNode(t, h.store)
	if err := h.store.TouchNodesAt(map[string]time.Time{uuid: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.waits.tunnelBudget = 200 * time.Millisecond
	parked := parkSignal(t, h)

	done := openTunnelAsync(context.Background(), h, uuid)
	parked(waitSiteSession)
	next := dualLaneSession(t, h, uuid)
	registerForTest(t, h, next)

	requireBudgetSpent(t, awaitBudgetResult(t, done), h.waits.tunnelBudget, "data lane did not reattach")
	if got := protocolCount(counters, "data_lane_unavailable"); got != 1 {
		t.Fatalf("data_lane_unavailable counted %d times, want 1", got)
	}
	if next.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("the backlog of a reconnect must not ride the new ctrl connection")
	}
}

// TestOpenTunnel_SessionFoundAtOnceKeepsItsRouting: a dual-lane session that
// is simply there (no wait) and has not attached its lane yet carries the
// tunnel on ctrl as before; only a backlog waits for the lane.
func TestOpenTunnel_SessionFoundAtOnceKeepsItsRouting(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	s := dualLaneSession(t, h, uuid)
	registerForTest(t, h, s)

	r := awaitBudgetResult(t, openTunnelAsync(context.Background(), h, uuid))
	if r.err != nil {
		t.Fatalf("OpenTunnel: %v", r.err)
	}
	if got := s.CtrlDataPlaneStreamCount(); got != 1 {
		t.Fatalf("ctrl data-plane streams = %d, want 1", got)
	}
}

// TestOpenTunnel_CallerDeadlineOnReplacementStaysTheCallers: when the caller's
// own deadline, shorter than the hub's budget, ends the wait on the
// replacement, the answer is the caller's context.DeadlineExceeded, as
// everywhere else: the budget did not run out, the caller stopped waiting.
func TestOpenTunnel_CallerDeadlineOnReplacementStaysTheCallers(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	h.waits.tunnelBudget = 5 * time.Second
	_ = detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := openTunnelAsync(ctx, h, uuid)
	parked(waitSiteDataLane)
	replacement := newDetachedSession(t, h, uuid)
	registerForTest(t, h, replacement)
	parked(waitSiteDataLane)

	r := awaitBudgetResult(t, done)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", r.err)
	}
	if errors.Is(r.err, ErrDataLaneUnavailable) {
		t.Fatalf("err = %v, must not be reported as the hub's budget running out", r.err)
	}
}

// TestOpenTunnel_ReplacementThatNeverAccepts: the node's next session
// registers with no data lane (so the tunnel rides its ctrl), but opening the
// stream there does not finish before the shared budget runs out. That is the
// hub's wait ending too: the typed lane error, not the deadline that the
// gateway would answer with a 502.
func TestOpenTunnel_ReplacementThatNeverAccepts(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	h.waits.tunnelBudget = 200 * time.Millisecond
	old := detachedSession(t, h, uuid)
	parked := parkSignal(t, h)

	// The replacement's OpenStream blocks until its ctx ends, the way a yamux
	// open does while the peer holds back its window.
	original := writeStreamHeaderFn
	t.Cleanup(func() { writeStreamHeaderFn = original })
	writeStreamHeaderFn = func(ctx context.Context, _ *yamux.Session, _ proto.StreamHeader) (net.Conn, bool, error) {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}

	done := openTunnelAsync(context.Background(), h, uuid)
	parked(waitSiteDataLane)
	_ = registeredSession(t, h, uuid) // closes old

	requireBudgetSpent(t, awaitBudgetResult(t, done), h.waits.tunnelBudget, "agent session did not accept the stream")
	if old.CtrlDataPlaneStreamCount() != 0 {
		t.Fatal("the replaced session must never carry a data-plane stream")
	}
}

// TestSessionWaits_WakeOnlyTheirOwnNode: a register or deregister wakes the
// waiters of that node and no other, so a fleet's reconnects do not wake
// every waiter in the hub; a node whose last waiter left holds no entry.
func TestSessionWaits_WakeOnlyTheirOwnNode(t *testing.T) {
	var w sessionWaits
	nextA, stopA := w.watch("a")
	nextB, stopB := w.watch("b")
	a, b := nextA(), nextB()

	w.notify("b")
	w.notify("c") // nobody waits for c
	select {
	case <-a:
		t.Fatal("a change of node b woke node a's waiter")
	default:
	}
	select {
	case <-b:
	default:
		t.Fatal("a change of node b did not wake node b's waiter")
	}
	if nextB() == b {
		t.Fatal("after a wake-up the waiter must get a fresh channel")
	}

	w.notify("a")
	select {
	case <-a:
	default:
		t.Fatal("a change of node a did not wake node a's waiter")
	}
	stopA()
	stopB()
	if got := w.watched(); got != 0 {
		t.Fatalf("%d node entries left after every waiter stopped, want 0", got)
	}
}

// TestOpenTunnel_LeavesNoWaiterBehind: a finished open that waited leaves no
// per-node wait entry.
func TestOpenTunnel_LeavesNoWaiterBehind(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	if err := h.store.TouchNodesAt(map[string]time.Time{uuid: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.waits.tunnelBudget = 100 * time.Millisecond
	r := awaitBudgetResult(t, openTunnelAsync(context.Background(), h, uuid))
	if !errors.Is(r.err, ErrDataLaneUnavailable) {
		t.Fatalf("err = %v, want ErrDataLaneUnavailable", r.err)
	}
	if got := h.waits.watched(); got != 0 {
		t.Fatalf("%d node wait entries left, want 0", got)
	}
}

// TestAwaitSessionID_EndsWhenSessionsStop: a hello waiting for its ctrl
// session gives up as soon as the hub stops its sessions, not after its whole
// wait.
func TestAwaitSessionID_EndsWhenSessionsStop(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	parked := parkSignal(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Session, 1)
	start := time.Now()
	go func() { done <- h.awaitSessionID(ctx, uuid, "some-id", 5*time.Second) }()
	parked(waitSiteHello)
	cancel()
	select {
	case s := <-done:
		if s != nil {
			t.Fatalf("awaitSessionID = %v, want nil", s)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("returned after %v, want at once", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("awaitSessionID ignored its context")
	}
}

func protocolCount(c *usagereport.Counters, name string) int {
	n := 0
	for _, count := range c.Drain()[usagereport.KindProtocol] {
		if count.Name == name {
			n += count.N
		}
	}
	return n
}
