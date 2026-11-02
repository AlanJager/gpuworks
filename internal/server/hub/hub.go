// Package hub is the server endpoint agents connect to. An agent dials the WS
// route, the connection is adapted to a net.Conn and a yamux session is layered
// on top; the agent then opens the long-lived ctrl stream. The hub authenticates
// per-node tokens, tracks a session table, and
// drives the ctrl protocol (hello/heartbeat/status). Desired-state push and RPC
// dispatch are built on the same Session.
package hub

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// Hub manages all agent sessions.
type Hub struct {
	outboundAllowPrivateHosts []string
	store                     *store.Store
	networks                  Networks
	log                       *slog.Logger
	now                       func() time.Time
	agentBinary               func(name string) ([]byte, error) // /dl/ artifacts, for pushed upgrades
	proxies                   *auth.ProxyTrust
	runtimeAssetLoader        func(packageDigest, modelID, file string) ([]byte, error)
	// enginePorts reads the controller's engine port range; nil sends none.
	enginePorts func() proto.PortRange
	// copies are the agents' latest copy progress frames; sessions own them.
	copies *prepobs.Copies

	// Metrics/log ingest sinks (set by the app; decoupled from the
	// tsdb/logstore packages).
	onMetrics         func(nodeUUID string, samples []proto.MetricSample)
	onLogs            func(nodeUUID string, lines []proto.LogLine)
	onBenchmarkDetail func(nodeUUID string, status proto.BenchmarkStatus)
	// onCardChecks and onNodeGone: card_checks.go.
	onCardChecks func(nodeUUID string, nodeID uint, at time.Time, gpus []proto.GPUInfo)
	onNodeGone   func(nodeUUID string)

	// The node queues persist per-node reports (status, heartbeats, image
	// presence) off the session read loops; see statusqueue.go for why those
	// loops must never wait on the store.
	statusQ *nodeQueue[queuedNodeStatus]
	touchQ  *nodeQueue[time.Time]

	// pushLimiter caps initial desired-state replays and network policy reads. A fleet
	// connecting at once otherwise runs one multi-query replay per session
	// against the store simultaneously, saturating its pool: status reports
	// (whose listener inventory the port allocator relies on) then apply
	// seconds late and allocations collide with listeners that exist but were
	// not yet reported.
	pushLimiter chan struct{}

	mu              sync.RWMutex
	sessions        map[string]*Session             // keyed by node UUID
	imageWatches    map[[2]uint]map[string]struct{} // active image/node watchers, owned by transfer id
	rejectMu        sync.Mutex
	rejects         []RejectEvent
	upgradeMu       sync.RWMutex
	upgradeErrors   map[string]string
	upgradeObserver func(agentupgrade.Observation)
	reconnectMu     sync.Mutex
	reconnect       map[string]reconnectState
	// usage counts agent-link failures for the usage report; nil counts
	// nothing.
	usage *usagereport.Counters
}

type reconnectState struct {
	failures int
	next     time.Time
}

// RejectEvent is a bounded, process-local record of a failed agent handshake.
// It deliberately contains only a token prefix so the diagnostic endpoint can
// help operators find a typo without turning the server into a credential log.
type RejectEvent struct {
	TokenPrefix string    `json:"token_prefix"`
	Reason      string    `json:"reason"`
	Remote      string    `json:"remote"`
	At          time.Time `json:"at"`
}

// SetRuntimeAssetLoader wires the catalog package boundary. Persisted desired
// state contains only immutable references; the hub loads bytes just before an
// authenticated push and only for agents that did not acknowledge the SHA.
func (h *Hub) SetRuntimeAssetLoader(loader func(packageDigest, modelID, file string) ([]byte, error)) {
	h.runtimeAssetLoader = loader
}

// SetUsageCounters wires the usage report's in-process counters. Call before
// the first agent connects.
func (h *Hub) SetUsageCounters(c *usagereport.Counters) { h.usage = c }

// SetSinks wires the metrics/log ingest callbacks.
func (h *Hub) SetSinks(onMetrics func(string, []proto.MetricSample), onLogs func(string, []proto.LogLine)) {
	h.onMetrics = onMetrics
	h.onLogs = onLogs
}

// SetBenchmarkDetailSink wires bounded gzip JSONL chunks from agent-origin
// benchmark runs into the server artifact store. The callback runs on the
// session reader goroutine and must do only bounded I/O.
func (h *Hub) SetBenchmarkDetailSink(fn func(string, proto.BenchmarkStatus)) {
	h.onBenchmarkDetail = fn
}

// New builds a Hub.
func New(st *store.Store, log *slog.Logger) *Hub {
	h := &Hub{
		store:         st,
		networks:      network.New(st, network.StoreCatalog{Store: st}),
		log:           log,
		now:           time.Now,
		sessions:      make(map[string]*Session),
		upgradeErrors: make(map[string]string),
		reconnect:     make(map[string]reconnectState),
		touchQ:        newTouchQueue(log, st),
		pushLimiter:   make(chan struct{}, initialPushConcurrency),
		copies:        prepobs.NewCopies(),
	}
	h.statusQ = newStatusQueue(h)
	return h
}

// RunNodeQueues drains the per-node report queues (status, heartbeats,
// image presence) until ctx ends. Call exactly once from the app's background
// loops; submitting works without it (bounded, and the next periodic report
// supersedes what was dropped), but reports would never persist. It returns
// once both workers have stopped, so shutdown never closes the store under a
// write in flight.
func (h *Hub) RunNodeQueues(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.statusQ.run(ctx) }()
	go func() { defer wg.Done(); h.touchQ.run(ctx) }()
	wg.Wait()
}

func (h *Hub) setUpgradeError(uuid, message string) {
	h.upgradeMu.Lock()
	defer h.upgradeMu.Unlock()
	if strings.TrimSpace(message) == "" {
		delete(h.upgradeErrors, uuid)
	} else {
		h.upgradeErrors[uuid] = message
	}
}

// UpgradeErrors returns the latest asynchronous upgrade failure by node UUID.
func (h *Hub) UpgradeErrors() map[string]string {
	h.upgradeMu.RLock()
	defer h.upgradeMu.RUnlock()
	out := make(map[string]string, len(h.upgradeErrors))
	for k, v := range h.upgradeErrors {
		out[k] = v
	}
	return out
}

func (h *Hub) recordReject(token, reason, remote string) {
	token = strings.TrimSpace(token)
	if len(token) > 8 {
		token = token[:8]
	}
	e := RejectEvent{TokenPrefix: token, Reason: strings.TrimSpace(reason), Remote: strings.TrimSpace(remote), At: time.Now().UTC()}
	h.usage.Inc(usagereport.KindProtocol, e.Reason)
	h.rejectMu.Lock()
	defer h.rejectMu.Unlock()
	if len(h.rejects) >= 64 {
		copy(h.rejects, h.rejects[1:])
		h.rejects = h.rejects[:63]
	}
	h.rejects = append(h.rejects, e)
}

// RecentRejects returns newest-first handshake rejection diagnostics.
func (h *Hub) RecentRejects() []RejectEvent {
	h.rejectMu.Lock()
	defer h.rejectMu.Unlock()
	out := make([]RejectEvent, len(h.rejects))
	for i := range h.rejects {
		out[len(h.rejects)-1-i] = h.rejects[i]
	}
	return out
}

// SetProxyTrust sets who may assert an agent's source via X-Forwarded-For.
// Without one XFF is ignored, so a direct client cannot spoof LastRemoteAddr.
func (h *Hub) SetProxyTrust(p *auth.ProxyTrust) { h.proxies = p }

// agentRemote is where an agent connected from: the peer, or behind a trusted
// proxy the last X-Forwarded-For hop that is not itself a listed proxy.
func (h *Hub) agentRemote(r *http.Request) string {
	if !h.proxies.Peer(r) {
		return r.RemoteAddr
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		forwarded := strings.TrimSpace(parts[i])
		if net.ParseIP(forwarded) != nil && !h.proxies.Listed(forwarded) {
			return forwarded
		}
	}
	return r.RemoteAddr
}

// SessionCount returns the number of live agent sessions (for /healthz, tests).
func (h *Hub) SessionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}

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
	lane := r.URL.Query().Get("lane")
	switch lane {
	case "", proto.LaneCtrl, proto.LaneData:
	default:
		http.Error(w, "unknown lane", http.StatusBadRequest)
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

// DialAgentWithSecret connects to an agent running in server_dial listen mode,
// proving knowledge of the install token that the agent was started with.
func (h *Hub) DialAgentWithSecret(ctx context.Context, addr, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("server_dial requires node token")
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("server_dial tcp dial: %w", err)
	}
	if err := netConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		_ = netConn.Close()
		return err
	}
	if err := h.serverDialServerAuth(netConn, secret); err != nil {
		_ = netConn.Close()
		return err
	}
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		_ = netConn.Close()
		return err
	}
	// server_dial keeps the single data-profile lane (no SessionID, no data
	// lane); dataLaneRoute's "none" state routes every kind over it — this is
	// its permanent shape, not a fallback from a failure.
	sess, cleanup, err := h.acceptYamuxServer(netConn, "", addr)
	if err != nil {
		_ = netConn.Close()
		return err
	}
	if sess.NodeUUID != "" {
		if err := h.store.SetNodeServerDialAddress(sess.NodeUUID, addr); err != nil {
			h.log.Warn("persist server_dial address failed", "node", sess.NodeUUID, "addr", addr, "err", err)
		}
	}
	go h.serveSession(sess, cleanup, addr)
	return nil
}

// RunServerDialReconnect redials registered reverse-connect nodes after server
// restarts and transient disconnects. Registered nodes authenticate with their
// stored per-node token.
func (h *Hub) RunServerDialReconnect(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	h.reconnectServerDial(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.reconnectServerDial(ctx)
		}
	}
}

func (h *Hub) reconnectServerDial(ctx context.Context) {
	nodes, err := h.store.ServerDialNodes()
	if err != nil {
		h.log.Warn("list server_dial nodes failed", "err", err)
		return
	}
	for i := range nodes {
		node := &nodes[i]
		if h.Online(node.UUID) {
			h.reconnectMu.Lock()
			delete(h.reconnect, node.UUID)
			h.reconnectMu.Unlock()
			continue
		}
		h.reconnectMu.Lock()
		state := h.reconnect[node.UUID]
		if !state.next.IsZero() && time.Now().Before(state.next) {
			h.reconnectMu.Unlock()
			continue
		}
		h.reconnectMu.Unlock()
		secret, ok := node.PlaintextToken()
		if !ok {
			// Legacy rows have no recoverable plaintext token. They must be
			// re-enrolled through the pending-node flow before server_dial can
			// reconnect.
			h.log.Warn("server_dial reconnect skipped: plaintext token unavailable", "node", node.UUID, "state", node.State)
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := h.DialAgentWithSecret(attemptCtx, node.ServerDialAddress, secret)
		cancel()
		if err != nil {
			h.reconnectMu.Lock()
			state.failures++
			delay := serverDialReconnectDelay(state.failures)
			state.next = time.Now().Add(delay)
			h.reconnect[node.UUID] = state
			h.reconnectMu.Unlock()
			h.log.Debug("server_dial reconnect failed", "node", node.UUID, "addr", node.ServerDialAddress, "err", err)
			continue
		}
		h.reconnectMu.Lock()
		delete(h.reconnect, node.UUID)
		h.reconnectMu.Unlock()
		h.log.Info("server_dial reconnected", "node", node.UUID, "addr", node.ServerDialAddress)
	}
}

func serverDialReconnectDelay(failures int) time.Duration {
	return recovery.Backoff(failures, 10*time.Second, 3, 5*time.Minute)
}

// ResetServerDialBackoff forgets a node's failed redials, so the next round
// dials it at once: recovery found the provider moved its agent.
func (h *Hub) ResetServerDialBackoff(uuid string) {
	h.reconnectMu.Lock()
	delete(h.reconnect, uuid)
	h.reconnectMu.Unlock()
}

// ServerDialRecoveryReady lets the SSH recovery layer wait until the cheaper
// reverse-dial path has reached its maximum backoff tier.
func (h *Hub) ServerDialRecoveryReady(uuid string) bool {
	h.reconnectMu.Lock()
	defer h.reconnectMu.Unlock()
	state, ok := h.reconnect[uuid]
	return ok && serverDialReconnectDelay(state.failures) >= 5*time.Minute
}

func (h *Hub) serverDialServerAuth(netConn net.Conn, secret string) error {
	line, err := readLine(netConn)
	if err != nil {
		return fmt.Errorf("read server_dial challenge: %w", err)
	}
	var challenge proto.ServerDialChallenge
	if err := json.Unmarshal(line, &challenge); err != nil {
		return fmt.Errorf("decode server_dial challenge: %w", err)
	}
	if strings.TrimSpace(challenge.Nonce) == "" {
		return errors.New("server_dial challenge missing nonce")
	}
	proofSecret := secret
	if n, lookupErr := h.store.NodeByUUID(challenge.NodeUUID); lookupErr == nil && n.Token != "" && hmac.Equal([]byte(secret), []byte(n.Token)) {
		proofSecret = proto.ServerDialNodeSecret(secret)
	}
	proof := proto.ServerDialProof{HMAC: proto.ServerDialHMAC(proofSecret, challenge.Nonce)}
	resp, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	resp = append(resp, '\n')
	if _, err := netConn.Write(resp); err != nil {
		return fmt.Errorf("write server_dial proof: %w", err)
	}
	return nil
}

// acceptYamuxServer builds the ctrl-side yamux session. lane selects the yamux
// profile: proto.LaneCtrl uses the fixed 256 KiB control profile and issues a
// random SessionID (returned in Registered) so the agent may open a data lane;
// an empty lane keeps the single adaptive data profile (legacy/single-lane and
// server_dial), which never issues a SessionID or opens a data lane.
func (h *Hub) acceptYamuxServer(netConn net.Conn, lane string, remoteOpt ...string) (*Session, func(), error) {
	profile := proto.LaneData
	if lane == proto.LaneCtrl {
		profile = proto.LaneCtrl
	}
	ycfg := laneYamuxConfig(profile, &slogWriter{log: h.log})
	ysess, err := yamux.Server(netConn, ycfg, nil)
	if err != nil {
		return nil, nil, err
	}

	// The agent opens the ctrl stream first.
	ctrl, err := ysess.AcceptStream()
	if err != nil {
		_ = ysess.Close()
		return nil, nil, err
	}
	hdr, err := readStreamHeader(ctrl)
	if err != nil || hdr.Kind != proto.StreamCtrl {
		_ = ysess.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("bad ctrl stream kind: %s", hdr.Kind)
	}

	sess := newSession(h, ysess, ctrl, remoteOpt...)
	if lane == proto.LaneCtrl {
		id, genErr := newSessionID()
		if genErr != nil {
			_ = ysess.Close()
			return nil, nil, genErr
		}
		sess.sessionID = id
	}
	// handshake registers the session (non-smoke) before it sends Registered.
	if err := sess.handshake(); err != nil {
		// go-yamux does not flush queued frames on Close; give the agent a
		// chance to read the reject/smoke reply written during handshake.
		sess.flushCtrlBeforeClose()
		_ = ysess.Close()
		return nil, nil, err
	}
	cleanup := func() {
		h.deregister(sess)
		_ = ysess.Close()
		_ = netConn.Close()
	}
	return sess, cleanup, nil
}

func (h *Hub) serveSession(sess *Session, cleanup func(), remote string) {
	defer cleanup()
	h.log.Info("agent connected", "node", sess.NodeUUID, "remote", remote)
	// Replay full desired state so the agent reconciles from a clean slate.
	// The replay is a multi-query read that storms the store when a fleet
	// connects at once; it must never delay this goroutine's read loop, or
	// every RPC to the node waits behind the replay (see push.go).
	go func() {
		if err := sess.pushInitialState(); err != nil {
			h.log.Warn("push initial state failed", "node", sess.NodeUUID, "err", err)
		}
	}()
	sess.ctrlLoop() // blocks until the stream closes
	h.log.Info("agent disconnected", "node", sess.NodeUUID)
}

// register places the session in the table, replacing any prior one.
func (h *Hub) register(s *Session) {
	// Handshakes already resolved this identity. Embedders may construct a
	// session directly; resolve those before taking the global session lock.
	if s.nodeID == 0 {
		if node, err := h.store.NodeByUUID(s.NodeUUID); err == nil {
			s.nodeID = node.ID
		}
	}
	h.mu.Lock()
	if old, ok := h.sessions[s.NodeUUID]; ok {
		old.Close()
	}
	h.copies.ForgetNode(s.nodeID)
	h.sessions[s.NodeUUID] = s
	h.mu.Unlock()
	_ = h.store.SetNodeState(s.NodeUUID, store.NodeStateOnline)
	_ = h.store.ClearNodeRecovery(s.NodeUUID)
	go h.resumeDirectImageTransfers(s.NodeUUID)
}

// deregister removes the session and marks the node offline (unless a newer
// session already replaced this one).
func (h *Hub) deregister(s *Session) {
	h.mu.Lock()
	if cur, ok := h.sessions[s.NodeUUID]; ok && cur == s {
		h.copies.ForgetNode(s.nodeID)
		delete(h.sessions, s.NodeUUID)
		h.mu.Unlock()
		h.statusQ.forget(s.NodeUUID) // a stale report would mark it online again
		_ = h.store.SetNodeState(s.NodeUUID, store.NodeStateOffline)
		if h.onNodeGone != nil {
			h.onNodeGone(s.NodeUUID)
		}
		return
	}
	h.mu.Unlock()
}

// authNodeToken authenticates the current per-node token. The token itself is
// the identity; a stale/unknown UUID cannot make a valid token fail.
func (h *Hub) authNodeToken(token string) (*store.Node, error) {
	n, err := h.store.NodeByTokenHash(store.HashToken(token))
	if err != nil {
		return nil, errBadToken
	}
	if !hmac.Equal([]byte(n.TokenHash), []byte(store.HashToken(token))) {
		return nil, errBadToken
	}
	return n, nil
}

// serverHello builds the hello the server sends back to the agent.
func (h *Hub) serverHello() proto.Hello {
	return proto.Hello{
		ProtoMin: proto.ProtoMin,
		ProtoMax: proto.ProtoMax,
		Version:  version.Version,
	}
}

// slogWriter adapts yamux's io.Writer logging into slog at debug level.
type slogWriter struct{ log *slog.Logger }

func (w *slogWriter) Write(p []byte) (int, error) {
	w.log.Debug("yamux", "msg", string(p))
	return len(p), nil
}

// SetOutboundAllowPrivateHosts sets administrator-controlled HTTP exceptions before the hub starts.
func (h *Hub) SetOutboundAllowPrivateHosts(hosts []string) {
	h.outboundAllowPrivateHosts = append([]string(nil), hosts...)
}
