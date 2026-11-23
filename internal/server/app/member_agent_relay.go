//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// memberAgentRelay is a member's use of hub.AgentRelay: the agent connections
// this member accepts stay here, each attached to the member that leads. When
// this member leads, the other end is its own hub over an in-process pipe;
// otherwise it is the leader's hub, reached on the member route.
type memberAgentRelay struct {
	forward *memberForward
	relay   *hub.AgentRelay
	// local runs an agent session of this member's hub on a connection;
	// remote is the agent's address for a request.
	local  func(conn net.Conn, lane, sessionID, remote string)
	remote func(*http.Request) string
}

func newMemberAgentRelay(f *memberForward, h *hub.Hub) *memberAgentRelay {
	m := &memberAgentRelay{forward: f, local: h.ServeRelayedConn, remote: h.AgentRemote}
	m.relay = hub.NewAgentRelay(f.log, m.attach, f.observedLeader)
	return m
}

// agentControlConnect reports an agent opening its control connection. Its
// data lane is an ordinary upgrade: it is bound to the session identity, so
// it reaches the leader like any other request and is opened again by the
// agent when the leader changes.
func agentControlConnect(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != agentConnectPath {
		return false
	}
	lane := r.URL.Query().Get("lane")
	return lane == "" || lane == proto.LaneCtrl
}

func (m *memberAgentRelay) serve(w http.ResponseWriter, r *http.Request) {
	m.relay.ServeWS(w, r, m.remote(r))
}

func (m *memberAgentRelay) attach(ctx context.Context, lane, sessionID, remote string) (net.Conn, uint64, bool, error) {
	leader, known := m.forward.observedLeader()
	if !known {
		return nil, 0, false, nil
	}
	if leader == m.forward.memberID() {
		near, far := net.Pipe()
		go m.local(far, lane, sessionID, remote)
		return near, leader, true, nil
	}
	proxy := m.forward.proxyTo(leader)
	if proxy == nil {
		return nil, 0, false, nil
	}
	m.forward.mu.Lock()
	address := m.forward.routes[leader]
	m.forward.mu.Unlock()
	target := url.URL{Scheme: "wss", Host: address, Path: agentConnectPath}
	if lane != "" {
		target.RawQuery = "lane=" + url.QueryEscape(lane)
	}
	header := http.Header{}
	// The leader records the agent's address, not this member's.
	if _, _, err := net.SplitHostPort(remote); err != nil {
		remote = net.JoinHostPort(remote, "0")
	}
	header.Set(memberForwardRemoteHeader, remote)
	if sessionID != "" {
		header.Set(memberForwardSessionHeader, sessionID)
	}
	//nolint:bodyclose // coder/websocket.Dial owns handshake bodies; NetConn below owns the upgraded connection.
	ws, _, err := websocket.Dial(ctx, target.String(), &websocket.DialOptions{HTTPClient: &http.Client{Transport: proxy.Transport}, HTTPHeader: header})
	if err != nil {
		return nil, 0, false, errors.Join(errors.New("leader did not accept the relayed agent session"), err)
	}
	return wsconn.Wrap(websocket.NetConn(context.Background(), ws, websocket.MessageBinary)), leader, true, nil
}
