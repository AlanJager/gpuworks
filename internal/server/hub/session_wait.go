package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// Waiting for a node's session, scoped to the node rather than to the one
// ctrl session a data-plane open happened to start on.
//
// An agent reconnect registers a new session and closes the previous one, so
// a tunnel that was waiting on the old session's detached data lane is woken
// with an error while the node's replacement session is already (or about to
// be) in the table. Opening the tunnel on the replacement serves the request
// instead of answering it with a 503 the reconnect was about to make
// unnecessary.
//
// Every wait of one open shares one budget (dataLaneReattachWait), bounded by
// the caller's context. A node with no session at all is waited for only if it
// was heard from recently; one that was not is answered at once, as before.

const (
	// nodeRecentlyOnlineWindow is how long after its last heartbeat a node
	// without a session is still treated as reconnecting. Only such a node
	// is waited for; one that has been gone longer answers at once instead
	// of holding every request for the wait budget. It spans two of the
	// agent's 15s heartbeats: deregister drops the session before it writes
	// last_seen_at, so in that gap the row still holds the last heartbeat,
	// which can be a whole interval old.
	nodeRecentlyOnlineWindow = 30 * time.Second
	// dataHelloSessionWait bounds how long a data lane's hello waits for the
	// ctrl session that issued its id to be registered on this member. It
	// covers a relayed ctrl session that arrives just after its agent's data
	// lane (a leader change), not a session that is not coming.
	dataHelloSessionWait = 3 * time.Second
)

// errCtrlSessionClosed is the awaitDataLane outcome "the ctrl session that
// owned this wait closed". It is still ErrDataLaneUnavailable (the lane is not
// available to this caller), but a distinguishable cause: the node may have,
// or be about to have, a replacement session, so the caller looks again
// instead of failing.
var errCtrlSessionClosed = errors.New("ctrl session closed while waiting for the data lane")

// Wait sites a test can see a waiter park at (sessionWaits.parkedHook).
const (
	waitSiteSession  = "session"
	waitSiteDataLane = "data_lane"
	waitSiteHello    = "hello"
)

// parked tells a test's hook that a waiter is about to block at site. It costs
// nothing when no hook is set, and a nil hub (a session built without one) has
// none.
func (h *Hub) parked(site string) {
	if h != nil && h.waits.parkedHook != nil {
		h.waits.parkedHook(site)
	}
}

// recentlyOnline reports whether the node heartbeat recently enough for a wait
// to be worthwhile. A node that never reported, or has been gone for longer
// than nodeRecentlyOnlineWindow, is not waited for.
func (h *Hub) recentlyOnline(uuid string) bool {
	if h.store == nil {
		return false
	}
	node, err := h.store.NodeByUUID(uuid)
	if err != nil || node.LastSeenAt == nil {
		return false
	}
	return h.now().Sub(*node.LastSeenAt) <= nodeRecentlyOnlineWindow
}

// tunnelWaitBudget is how long an OpenTunnel call waits in all: the detached
// data lane's reattach wait, unless a test set a shorter one.
func (h *Hub) tunnelWaitBudget() time.Duration {
	if h.waits.tunnelBudget > 0 {
		return h.waits.tunnelBudget
	}
	return dataLaneReattachWait
}

// dataHelloWaitBudget is how long a data-lane hello waits for its ctrl
// session to register, unless a test set a shorter one.
func (h *Hub) dataHelloWaitBudget() time.Duration {
	if h.waits.helloBudget > 0 {
		return h.waits.helloBudget
	}
	return dataHelloSessionWait
}

// openTunnel opens hdr on the node's session within one budget
// (tunnelWaitBudget, never longer than ctx), which every wait the open meets
// shares: for the node to have a session at all, for the data lane of the
// session it opens on, and for the node's next session when that one closes
// under it — an agent that drops again is followed to its next session too.
// However the reconnect unfolds, the caller waits the budget once.
//
// When the budget runs out the answer is ErrDataLaneUnavailable, the
// retryable 503, and never the budget's own deadline, which the gateway would
// read as the caller's and answer with a 502. The caller's own cancel or
// deadline stays the caller's error.
func (h *Hub) openTunnel(ctx context.Context, uuid string, hdr proto.StreamHeader) (net.Conn, error) {
	waitCtx, cancel := context.WithTimeout(ctx, h.tunnelWaitBudget())
	defer cancel()
	var prev *Session
	for {
		s, waited, err := h.awaitSession(ctx, waitCtx, uuid, prev)
		if err != nil {
			return nil, err
		}
		// A session the open had to wait for is the agent's reconnect, and
		// the requests that waited for it are a backlog. If the agent opens a
		// data lane (the session was issued an id for one), the backlog waits
		// for that lane in the rest of the budget instead of moving onto the
		// new ctrl connection the lane exists to keep clear (ADR 0024). A
		// session found at once keeps its usual routing.
		awaitLane := waited && s.sessionID != "" && s.DataLaneState() == "none"
		conn, err := s.openTunnelStream(waitCtx, hdr, awaitLane)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		if waitCtx.Err() == nil && (errors.Is(err, errCtrlSessionClosed) || s.isClosed()) {
			prev = s
			continue
		}
		return nil, h.tunnelBudgetSpent(waitCtx, s, awaitLane, err)
	}
}

// openTunnelStream opens hdr on s, first waiting for s's data lane to attach
// when awaitLane is set.
func (s *Session) openTunnelStream(ctx context.Context, hdr proto.StreamHeader, awaitLane bool) (net.Conn, error) {
	if awaitLane {
		if _, err := s.awaitDataLane(ctx); err != nil {
			return nil, err
		}
	}
	return s.openStream(ctx, hdr)
}

// tunnelBudgetSpent reports a failure that came from the open's budget running
// out (a wait or an OpenStream ended by waitCtx's deadline, the caller still
// waiting) as ErrDataLaneUnavailable, and counts it when it was the data lane
// that never came. Any other failure keeps its own cause.
func (h *Hub) tunnelBudgetSpent(waitCtx context.Context, s *Session, awaitLane bool, err error) error {
	if waitCtx.Err() == nil || !isTimeout(err) {
		return err
	}
	budget := h.tunnelWaitBudget()
	if awaitLane || s.DataLaneState() == "detached" {
		h.usage.Inc(usagereport.KindProtocol, "data_lane_unavailable")
		return fmt.Errorf("%w: node %s data lane did not reattach within %s", ErrDataLaneUnavailable, s.NodeUUID, budget)
	}
	return fmt.Errorf("%w: node %s agent session did not accept the stream within %s", ErrDataLaneUnavailable, s.NodeUUID, budget)
}

// isTimeout reports a deadline failure: the context's own, or yamux's i/o
// deadline (a net.Error whose Timeout is true).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// awaitSession returns the node's session, other than except when one is
// given, waiting for one to be registered. waited reports whether the session
// came after a wait: it had to block for it, or except (a session that closed
// under the caller) shows the caller has been waiting through a reconnect.
// It returns at once when a usable session is already there, and — for a node
// with no session at all — when the node was not seen recently, which is the
// pre-existing "node not connected" answer. The wait ends at waitCtx's
// deadline or at ctx's, whichever comes first.
func (h *Hub) awaitSession(ctx, waitCtx context.Context, uuid string, except *Session) (s *Session, waited bool, err error) {
	if s = h.session(uuid); s != nil && s != except {
		return s, except != nil, nil
	}
	// Whether the node is worth waiting for is judged once, on entry, and not
	// on each wake-up: a node that was recent on entry stays waited for until
	// the budget ends. One that lost a session (except) was just connected.
	if s == nil && except == nil && !h.recentlyOnline(uuid) {
		return nil, false, errors.New("node not connected: " + uuid)
	}
	next, stop := h.waits.watch(uuid)
	defer stop()
	for {
		changed := next()
		if s = h.session(uuid); s != nil && s != except {
			return s, true, nil
		}
		h.parked(waitSiteSession)
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return nil, true, err
			}
			return nil, true, fmt.Errorf("%w: node %s has no live agent session", ErrDataLaneUnavailable, uuid)
		}
	}
}

// awaitSessionID returns the node's session when it presents sessionID,
// waiting up to budget, or until ctx ends, for one to register. A session
// registered under another id ends the wait at once: the presented id names a
// ctrl session that is gone, and no later registration can make it match
// again.
func (h *Hub) awaitSessionID(ctx context.Context, uuid, sessionID string, budget time.Duration) *Session {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	next, stop := h.waits.watch(uuid)
	defer stop()
	for {
		changed := next()
		if s := h.session(uuid); s != nil {
			if s.sessionID == sessionID {
				return s
			}
			return nil
		}
		h.parked(waitSiteHello)
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	}
}
