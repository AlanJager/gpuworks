package hub

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// ServeWS is the HTTP handler for /api/v1/agent/connect.
//
// The lane query parameter selects the connection role (plan §3.1):
//
//   - absent: legacy/single-lane. One yamux session with the data-lane profile
//     carries every kind. No SessionID is issued.
//   - lane=ctrl: the dedicated control connection (ctrl-lane profile). A random
//     SessionID is issued in Registered so the agent can open a data lane.
//   - lane=data: a data-lane WebSocket. The agent authenticates with a DataHello
//     line before yamux; on success it attaches to the ctrl session.
//
// An unknown lane value is a 400 before the WebSocket upgrade.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	sessionCtx, done, ok := h.sessionWork.begin()
	if !ok {
		messages.WriteHTTPError(w, http.StatusServiceUnavailable, errSessionsStopped)
		return
	}
	defer done()
	lane := r.URL.Query().Get("lane")
	switch lane {
	case "", proto.LaneCtrl, proto.LaneData:
	default:
		messages.WriteHTTPError(w, http.StatusBadRequest, errors.New("unknown lane"))
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Agents are not browsers; skip origin checks.
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Warn("ws accept failed", "err", err)
		return
	}
	// No overall deadline: this is a long-lived connection. yamux keepalive and
	// the application heartbeat detect death.
	ctx := context.Background()
	netConn := wsconn.Wrap(websocket.NetConn(ctx, c, websocket.MessageBinary))
	stopClose := context.AfterFunc(sessionCtx, func() { _ = netConn.Close() })
	defer stopClose()

	remote := h.agentRemote(r)
	if lane == proto.LaneData {
		h.serveDataLane(netConn, c, remote)
		return
	}
	sess, cleanup, err := h.acceptYamuxServer(netConn, lane, remote)
	if err != nil {
		if errors.Is(err, errSmokeHandshake) {
			_ = netConn.Close()
			_ = c.Close(websocket.StatusNormalClosure, "smoke")
			return
		}
		attrs := []any{"err", err, "remote", remote}
		var rejected *handshakeRejectError
		if errors.As(err, &rejected) {
			attrs = append(attrs, "reason", rejected.reason, "token_prefix", rejected.token)
		}
		h.log.Warn("agent session failed", attrs...)
		_ = netConn.Close()
		_ = c.Close(websocket.StatusInternalError, "session")
		return
	}
	h.serveSession(sess, cleanup, remote)
}
