package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestSession_DetachDataIfCurrentWakesWaiterForRetryAndClosesTheDeadSession is
// a white-box test of the dead-session race handling: openStream calls
// detachDataIfCurrent only once it has confirmed (via writeStreamHeader's
// dead return) that a data lane truly died. This test drives that state
// transition directly on a session that genuinely still reports
// IsClosed()==false (a TCP death yamux has not yet reflected there) and
// asserts three things: a caller parked in awaitDataLane wakes up and gets
// the next attached session (never ctrl), and — the fix this test guards —
// detachDataIfCurrent also closes the session it just detached, so it never
// lingers half-open once the code has already decided it is dead.
func TestSession_DetachDataIfCurrentWakesWaiterForRetryAndClosesTheDeadSession(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	dead, _ := yamuxPairData(t)
	sess.attachData(dead)
	if dead.IsClosed() {
		t.Fatal("dead session must still report healthy (IsClosed()==false) for this to test the race path, not a plain clean drop")
	}
	// This is the exact sequence openStream follows once writeStreamHeader
	// has confirmed death: detach (and close) the session that looked
	// healthy first, then wait — never the other way around, or
	// awaitDataLane's fast path would just hand back the (still nominally
	// attached) dead session.
	sess.detachDataIfCurrent(dead)
	if sess.DataLaneState() != "detached" {
		t.Fatalf("state after detachDataIfCurrent = %q, want detached", sess.DataLaneState())
	}
	if !dead.IsClosed() {
		t.Fatal("detachDataIfCurrent must close the session it confirmed dead, not leave it half-open")
	}

	type result struct {
		ysess *yamux.Session
		err   error
	}
	done := make(chan result, 1)
	go func() {
		ysess, err := sess.awaitDataLane(context.Background())
		done <- result{ysess, err}
	}()
	time.Sleep(30 * time.Millisecond)

	fresh, _ := yamuxPairData(t)
	sess.attachData(fresh)

	select {
	case r := <-done:
		if r.err != nil || r.ysess != fresh {
			t.Fatalf("awaitDataLane after detachDataIfCurrent: err=%v ysess-is-fresh=%v, want the freshly attached session with no error", r.err, r.ysess == fresh)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was never woken by detachDataIfCurrent")
	}
}

// TestWriteStreamHeader_ClosedSessionReportsDead is a direct, deterministic
// unit test of writeStreamHeader's death classification for the "OpenStream
// itself fails" branch: once a session is genuinely closed, IsClosed() is
// guaranteed true (yamux sets its shutdown flag, and only then closes
// shutdownCh, before any error caused by that shutdown is ever returned to a
// caller), so writeStreamHeader must report dead=true.
func TestWriteStreamHeader_ClosedSessionReportsDead(t *testing.T) {
	dead, _ := yamuxPairData(t)
	_ = dead.Close()

	conn, isDead, err := writeStreamHeader(context.Background(), dead, proto.StreamHeader{Kind: proto.StreamTunnel})
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("OpenStream on an already-closed session must fail")
	}
	if !isDead {
		t.Fatal("dead = false, want true: OpenStream failing on a closed session must be reported as confirmed death")
	}
}

// TestWriteStreamHeader_CtxCanceledDoesNotReportDead is the direct
// counterpart proving the other side of the classification: a session that
// is still healthy but whose OpenStream call is canceled by ctx (backlog
// exhausted, so the call is guaranteed to block until the cancel, not race
// against it) must report dead=false. Detaching a healthy lane over a
// canceled request would turn one canceled request into node-wide
// data-plane unavailability, which is exactly the bug this guards against.
func TestWriteStreamHeader_CtxCanceledDoesNotReportDead(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	cfg := laneYamuxConfig(proto.LaneData, io.Discard)
	cfg.AcceptBacklog = 1
	healthy, err := yamux.Server(a, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = healthy.Close() })
	peer, err := yamux.Client(b, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	// Occupy the one backlog slot so the next OpenStream is guaranteed to
	// block waiting for a syn slot rather than racing to succeed first.
	if _, err := healthy.OpenStream(context.Background()); err != nil {
		t.Fatalf("priming stream: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	conn, isDead, err := writeStreamHeader(ctx, healthy, proto.StreamHeader{Kind: proto.StreamTunnel})
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if isDead {
		t.Fatal("dead = true, want false: a canceled OpenStream on a healthy session must not be reported as death")
	}
	if healthy.IsClosed() {
		t.Fatal("the session must remain open: only the OpenStream call was canceled")
	}
}

// TestSession_OpenStream_CtxCanceledOnAttachedLaneDoesNotDetach is the
// regression test for the bug this file was rewritten to guard: a caller
// whose context is canceled while waiting to open a stream on an attached,
// healthy data lane must get its ctx error back untouched — the lane must
// stay attached and open, and ctrl must never carry the stream. Before the
// fix, any writeStreamHeader failure (ctx cancellation included) detached
// the lane without closing it, so a single canceled request degraded the
// whole node's data plane to a 5-second wait (or a 503) on every subsequent
// stream until the lane happened to drop for a real reason.
func TestSession_OpenStream_CtxCanceledOnAttachedLaneDoesNotDetach(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, ctrlClient := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	var ctrlStreams atomic.Int32
	go func() {
		for {
			st, err := ctrlClient.AcceptStream()
			if err != nil {
				return
			}
			ctrlStreams.Add(1)
			_ = st.Close()
		}
	}()

	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	cfg := laneYamuxConfig(proto.LaneData, io.Discard)
	cfg.AcceptBacklog = 1
	live, err := yamux.Server(a, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	peer, err := yamux.Client(b, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := live.OpenStream(context.Background()); err != nil {
		t.Fatalf("priming stream: %v", err)
	}

	sess.attachData(live)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	conn, err := sess.openStream(ctx, proto.StreamHeader{Kind: proto.StreamLogtail, Ref: "canceled"})
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := sess.DataLaneState(); got != "attached" {
		t.Fatalf("DataLaneState = %q, want attached: a canceled request must not detach a healthy lane", got)
	}
	sess.dataMu.Lock()
	same := sess.data == live
	sess.dataMu.Unlock()
	if !same {
		t.Fatal("the healthy lane was replaced/detached by a request that was merely canceled")
	}
	if live.IsClosed() {
		t.Fatal("the healthy data lane must not be closed by a canceled request")
	}
	if ctrlStreams.Load() != 0 {
		t.Fatalf("ctrl carried %d data-plane stream(s); a canceled request must never fall back to ctrl", ctrlStreams.Load())
	}
}

// TestSession_OpenStream_NonDeathTimeoutDoesNotDetachOrCloseTheLane is the
// same non-death property as the ctx-cancellation test above, but for a
// request that has no ctx deadline of its own and simply rides out
// dataLaneReattachWait-shaped blocking inside OpenStream — a data lane whose
// local open-backlog is pinned to 1 and already exhausted by a stream nobody
// ever accepts or acknowledges. That leaves the session perfectly healthy
// from IsClosed()'s perspective; the next OpenStream call can only ever
// block until ctx fires, which here is a plain timeout rather than an
// explicit cancel. Timing out this way must not detach or close the lane.
func TestSession_OpenStream_NonDeathTimeoutDoesNotDetachOrCloseTheLane(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, ctrlClient := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	var ctrlStreams atomic.Int32
	go func() {
		for {
			st, err := ctrlClient.AcceptStream()
			if err != nil {
				return
			}
			ctrlStreams.Add(1)
			_ = st.Close()
		}
	}()

	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	cfg := laneYamuxConfig(proto.LaneData, io.Discard)
	cfg.AcceptBacklog = 1
	live, err := yamux.Server(a, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	peer, err := yamux.Client(b, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	// Occupy the one backlog slot; nobody ever accepts or acks it, so it
	// stays inflight forever and the next OpenStream has nowhere to go but
	// to wait out ctx.
	if _, err := live.OpenStream(context.Background()); err != nil {
		t.Fatalf("priming stream: %v", err)
	}

	sess.attachData(live)
	if live.IsClosed() {
		t.Fatal("primed session must still report healthy")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	conn, err := sess.openStream(ctx, proto.StreamHeader{Kind: proto.StreamLogtail, Ref: "non-death-timeout"})
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := sess.DataLaneState(); got != "attached" {
		t.Fatalf("DataLaneState = %q, want attached: a plain ctx timeout must not detach a healthy lane", got)
	}
	sess.dataMu.Lock()
	same := sess.data == live
	sess.dataMu.Unlock()
	if !same {
		t.Fatal("the healthy lane was replaced/detached by a request that merely timed out")
	}
	if live.IsClosed() {
		t.Fatal("the healthy data lane must not be closed by a request that merely timed out")
	}
	if ctrlStreams.Load() != 0 {
		t.Fatalf("ctrl carried %d data-plane stream(s); a timed-out request must never fall back to ctrl", ctrlStreams.Load())
	}
}

// TestSession_OpenStream_SessionShutdownWhileOpeningDetachesClosesAndRetries
// drives Session.openStream through a confirmed-dead session end to end,
// deterministically: a data lane whose backlog is pinned to 1 and already
// exhausted, so the next OpenStream call is guaranteed to block waiting for
// a syn slot (not race to succeed first) — and then, from a separate
// goroutine, the session is closed out from under that blocked call. That
// deterministically takes OpenStream's "shutdownCh closed while waiting"
// branch, at which point IsClosed() is guaranteed true, so this is death
// confirmed the same way production code confirms it — not a canceled ctx,
// not a resource limit. openStream must detach and close that session
// (never leaving it half-open) and, once a fresh lane reattaches, retry
// successfully against it — all without ctrl ever carrying the stream.
func TestSession_OpenStream_SessionShutdownWhileOpeningDetachesClosesAndRetries(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, ctrlClient := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	var ctrlStreams atomic.Int32
	go func() {
		for {
			st, err := ctrlClient.AcceptStream()
			if err != nil {
				return
			}
			ctrlStreams.Add(1)
			_ = st.Close()
		}
	}()

	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	cfg := laneYamuxConfig(proto.LaneData, io.Discard)
	cfg.AcceptBacklog = 1
	dying, err := yamux.Server(a, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := yamux.Client(b, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := dying.OpenStream(context.Background()); err != nil {
		t.Fatalf("priming stream: %v", err)
	}

	sess.attachData(dying)
	if dying.IsClosed() {
		t.Fatal("the lane must still be healthy before the deliberate close below")
	}

	// dying's OpenStream call (triggered by openStream below) is guaranteed
	// to already be blocked on the exhausted backlog by the time this fires:
	// nothing else can unblock it, and there is no ctx deadline racing it.
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = dying.Close()
	}()
	fresh, freshClient := yamuxPairData(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		sess.attachData(fresh)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := sess.openStream(ctx, proto.StreamHeader{Kind: proto.StreamLogtail, Ref: "shutdown-retry"})
	if err != nil {
		t.Fatalf("openStream after session shutdown = %v, want success against the reattached lane", err)
	}
	defer conn.Close()

	stream, err := freshClient.AcceptStream()
	if err != nil {
		t.Fatalf("reattached lane never received the retried stream: %v", err)
	}
	defer stream.Close()
	var header proto.StreamHeader
	if err := json.NewDecoder(stream).Decode(&header); err != nil {
		t.Fatal(err)
	}
	if header.Kind != proto.StreamLogtail || header.Ref != "shutdown-retry" {
		t.Fatalf("header = %+v", header)
	}

	if !dying.IsClosed() {
		t.Fatal("the confirmed-dead session must be closed, not left half-open")
	}
	if got := sess.DataLaneState(); got != "attached" {
		t.Fatalf("state after retry = %q, want attached (the fresh lane)", got)
	}
	if ctrlStreams.Load() != 0 {
		t.Fatalf("ctrl carried %d data-plane stream(s); a confirmed-dead lane must never fall back to ctrl", ctrlStreams.Load())
	}
}

// A data lane that dies, reattaches and then dies again on the very next write
// is still "this node's data lane was not usable". openStream used to return
// that second failure raw, which the proxy can only read as a dead upstream
// (502); the dual-lane contract promises the typed, retryable
// ErrDataLaneUnavailable (ADR 0024), which the proxy answers as 503. Staging
// the two deaths through the write seam keeps this deterministic: making a
// real yamux session look healthy at the route check and dead at the write is
// a race no test can reproduce reliably.
func TestSession_OpenStream_ReattachedLaneDyingAgainIsReportedUnavailable(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, ctrlClient := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	var ctrlStreams atomic.Int32
	go func() {
		for {
			st, err := ctrlClient.AcceptStream()
			if err != nil {
				return
			}
			ctrlStreams.Add(1)
			_ = st.Close()
		}
	}()

	first, _ := yamuxPairData(t)
	sess.attachData(first)
	second, _ := yamuxPairData(t)

	original := writeStreamHeaderFn
	t.Cleanup(func() { writeStreamHeaderFn = original })
	var wrote []string
	writeStreamHeaderFn = func(ctx context.Context, ysess *yamux.Session, hdr proto.StreamHeader) (net.Conn, bool, error) {
		switch ysess {
		case first:
			wrote = append(wrote, "first")
			// The agent's lane reconnects while this write is failing, so the
			// retry has a freshly attached lane to run on.
			sess.attachData(second)
			return nil, true, errors.New("yamux: session shutdown")
		case second:
			wrote = append(wrote, "second")
			return nil, true, errors.New("yamux: session shutdown")
		default:
			wrote = append(wrote, "ctrl")
			return original(ctx, ysess, hdr)
		}
	}

	conn, err := sess.openStream(context.Background(), proto.StreamHeader{Kind: proto.StreamLogtail, Ref: "dies-twice"})
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("err = %v, want ErrDataLaneUnavailable so the proxy answers a retryable 503 rather than 502", err)
	}
	if strings.Join(wrote, ",") != "first,second" {
		t.Fatalf("writes = %v, want the first lane then the reattached one, and never ctrl", wrote)
	}
	if got := sess.DataLaneState(); got != "detached" {
		t.Fatalf("DataLaneState = %q, want detached: the lane that failed again must not stay current", got)
	}
	if ctrlStreams.Load() != 0 {
		t.Fatalf("ctrl carried %d data-plane stream(s); a dead data lane must never fall back to ctrl", ctrlStreams.Load())
	}
}
