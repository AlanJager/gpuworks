package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	yamux "github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// An agent keeps one long connection to the control plane. In a cluster the
// member that accepted it is not always the member that serves business: that
// is the one leading the database, and it changes. AgentRelay lets the agent
// keep its connection across such a change. The accepting member terminates
// the agent's connection and holds, behind it, a session with whichever
// member serves now. To that member the relay looks like the agent: it
// repeats the agent's hello, and passes control lines and streams both ways.
// When the serving member changes, the relay drops that session and opens one
// with the new member; the agent sees the control plane send its desired
// state again, as it does after any reconnect, and nothing else.
//
// What does not survive the change is what was in flight through the old
// member: calls awaiting an answer and open streams (tunnels, log tails,
// terminals, transfers).

const (
	// A control line carries at most one desired-state replay with its
	// runtime assets.
	relayLineLimit     = 64 << 20
	relayAttachRetry   = 250 * time.Millisecond
	relayTargetRecheck = 250 * time.Millisecond
	relayOpenTimeout   = 15 * time.Second
)

// RelayAttach opens the control-plane end for one relayed agent connection:
// a connection on which the serving member runs the ordinary agent session.
// target names that member, so the relay can tell when another one serves.
// sessionID is the identity the relay gave the agent; the serving member
// adopts it, so the agent's data lane still finds its session. remote is the
// agent's address. ok is false while no member is known to serve.
type RelayAttach func(ctx context.Context, lane, sessionID, remote string) (conn net.Conn, target uint64, ok bool, err error)

// RelayTarget reports the member that serves now, and whether one is known.
type RelayTarget func() (uint64, bool)

// AgentRelay accepts agent control connections on a member and keeps each
// attached to the member that serves.
type AgentRelay struct {
	log    *slog.Logger
	attach RelayAttach
	target RelayTarget

	mu     sync.Mutex
	closed bool
	conns  map[*relayedAgent]struct{}
	work   sync.WaitGroup
}

func NewAgentRelay(log *slog.Logger, attach RelayAttach, target RelayTarget) *AgentRelay {
	return &AgentRelay{log: log, attach: attach, target: target, conns: map[*relayedAgent]struct{}{}}
}

type relayedSessionKey struct{}

// WithRelayedSessionID marks a request as a relay's control-plane end. Only
// the member route may set it: a client cannot choose its session identity.
func WithRelayedSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, relayedSessionKey{}, sessionID)
}

func relayedSessionID(ctx context.Context) string {
	id, _ := ctx.Value(relayedSessionKey{}).(string)
	return id
}

// Count reports the agent connections the relay holds.
func (r *AgentRelay) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// Close ends every relayed connection and waits for them; their agents dial
// again. Later connections are refused.
func (r *AgentRelay) Close() {
	r.mu.Lock()
	r.closed = true
	conns := make([]*relayedAgent, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
	r.work.Wait()
}

// ServeWS accepts an agent's control connection (the ctrl lane, or the
// single lane of an older agent) and relays it until either end is done.
func (r *AgentRelay) ServeWS(w http.ResponseWriter, req *http.Request, remote string) {
	lane := req.URL.Query().Get("lane")
	if lane != "" && lane != proto.LaneCtrl {
		messages.WriteHTTPError(w, http.StatusBadRequest, errors.New("unknown lane"))
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		messages.WriteHTTPError(w, http.StatusServiceUnavailable, errSessionsStopped)
		return
	}
	r.work.Add(1)
	r.mu.Unlock()
	defer r.work.Done()
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true}) // agents are not browsers
	if err != nil {
		return
	}
	netConn := wsconn.Wrap(websocket.NetConn(context.Background(), c, websocket.MessageBinary))
	defer func() { _ = netConn.Close() }()
	profile := proto.LaneData
	if lane == proto.LaneCtrl {
		profile = proto.LaneCtrl
	}
	agent, err := yamux.Server(netConn, laneYamuxConfig(profile, nil), nil)
	if err != nil {
		return
	}
	defer func() { _ = agent.Close() }()
	ctrl, err := agent.AcceptStream()
	if err != nil {
		return
	}
	in := bufio.NewReaderSize(ctrl, 64<<10)
	header, err := relayLine(in)
	if err != nil {
		return
	}
	hello, err := relayLine(in)
	if err != nil {
		return
	}
	var env proto.Envelope
	var facts proto.Hello
	if json.Unmarshal(hello, &env) != nil || env.T != proto.THello || json.Unmarshal(env.D, &facts) != nil {
		return
	}
	a := &relayedAgent{relay: r, lane: lane, remote: remote, agent: agent, ctrl: ctrl, in: in, header: header, hello: hello, once: facts.Smoke, done: make(chan struct{})}
	if lane == proto.LaneCtrl {
		if a.sessionID, err = newSessionID(); err != nil {
			return
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.conns[a] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, a)
		r.mu.Unlock()
	}()
	a.run()
}

func relayLine(in *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := in.ReadSlice('\n')
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) || len(line) > relayLineLimit {
			return nil, err
		}
	}
}

// relayedAgent is one agent connection and its current control-plane end.
type relayedAgent struct {
	relay     *AgentRelay
	lane      string
	remote    string
	sessionID string
	agent     *yamux.Session
	ctrl      io.ReadWriteCloser // the agent's control stream
	in        *bufio.Reader
	header    []byte // the control stream's header line, as the agent sent it
	hello     []byte // the agent's hello line, repeated to each serving member
	// once is set for a handshake that only checks the path: it is relayed
	// to one member and never attached again.
	once bool

	writeMu  sync.Mutex // serializes control lines to the agent
	mu       sync.Mutex
	current  *relayEnd
	changed  chan struct{} // closed when current changes
	closing  sync.Once
	done     chan struct{}
	attached bool // the agent has been told it is registered
}

// relayEnd is the session with one serving member.
type relayEnd struct {
	target uint64
	conn   net.Conn
	sess   *yamux.Session
	ctrl   *yamux.Stream
	gone   chan struct{}
	end    sync.Once
}

func (e *relayEnd) close() {
	e.end.Do(func() {
		close(e.gone)
		_ = e.sess.Close()
		_ = e.conn.Close()
	})
}

func (a *relayedAgent) close() {
	a.closing.Do(func() {
		close(a.done)
		_ = a.agent.Close()
		a.mu.Lock()
		end := a.current
		a.mu.Unlock()
		if end != nil {
			end.close()
		}
	})
}

func (a *relayedAgent) run() {
	defer a.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-a.done:
		case <-a.agent.CloseChan():
			a.close()
		}
		cancel()
	}()
	a.mu.Lock()
	a.changed = make(chan struct{})
	a.mu.Unlock()
	go a.agentLines()
	go a.agentStreams()
	for ctx.Err() == nil {
		end, verdict := a.attachOnce(ctx)
		if verdict == relayRefused {
			// The serving member turned the agent away, or the path check
			// is done. Closing the session does not send what is queued:
			// let the agent read the answer first.
			a.flushAgent()
			return
		}
		if end == nil {
			select {
			case <-ctx.Done():
			case <-time.After(relayAttachRetry):
			}
			continue
		}
		a.serveEnd(ctx, end)
		if a.once {
			return
		}
	}
}

type relayVerdict int

const (
	relayRetry relayVerdict = iota
	relayAttached
	relayRefused
)

// attachOnce opens a session with the serving member and repeats the agent's
// hello. The first time, the member's answer goes to the agent as it is. A
// later time the agent is already registered: the member's confirmation is
// read and kept from it, and a refusal ends the agent's connection.
func (a *relayedAgent) attachOnce(ctx context.Context) (*relayEnd, relayVerdict) {
	dial, cancel := context.WithTimeout(ctx, relayOpenTimeout)
	defer cancel()
	conn, target, ok, err := a.relay.attach(dial, a.lane, a.sessionID, a.remote)
	if err != nil || !ok {
		return nil, relayRetry
	}
	profile := proto.LaneData
	if a.lane == proto.LaneCtrl {
		profile = proto.LaneCtrl
	}
	sess, err := yamux.Client(conn, laneYamuxConfig(profile, nil), nil)
	if err != nil {
		_ = conn.Close()
		return nil, relayRetry
	}
	end := &relayEnd{target: target, conn: conn, sess: sess, gone: make(chan struct{})}
	ctrl, err := sess.OpenStream(dial)
	if err != nil {
		end.close()
		return nil, relayRetry
	}
	end.ctrl = ctrl
	if _, err := ctrl.Write(append(append([]byte(nil), a.header...), a.hello...)); err != nil {
		end.close()
		return nil, relayRetry
	}
	in := bufio.NewReaderSize(ctrl, 64<<10)
	// The handshake answer is `registered` then the member's own hello, or a
	// single `reject`.
	for answered := 0; answered < 2; answered++ {
		line, err := relayLine(in)
		if err != nil {
			end.close()
			if !a.attached || a.once {
				return nil, relayRefused // the member closed a first handshake: so does the agent's connection
			}
			return nil, relayRetry
		}
		var env proto.Envelope
		_ = json.Unmarshal(line, &env)
		if !a.attached || env.T == proto.TReject {
			if a.writeAgent(line) != nil || env.T == proto.TReject {
				end.close()
				return nil, relayRefused
			}
		}
	}
	a.attached = true
	a.mu.Lock()
	a.current = end
	changed := a.changed
	a.changed = make(chan struct{})
	a.mu.Unlock()
	close(changed)
	go a.memberLines(end, in)
	go a.memberStreams(end)
	return end, relayAttached
}

// serveEnd holds one session until it ends or another member serves.
func (a *relayedAgent) serveEnd(ctx context.Context, end *relayEnd) {
	ticker := time.NewTicker(relayTargetRecheck)
	defer ticker.Stop()
	defer func() {
		a.mu.Lock()
		if a.current == end {
			a.current = nil
		}
		a.mu.Unlock()
		end.close()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-end.gone:
			return
		case <-end.sess.CloseChan():
			return
		case <-ticker.C:
			if a.relay.target == nil {
				continue
			}
			if serving, known := a.relay.target(); known && serving != end.target {
				if a.relay.log != nil {
					a.relay.log.Info("agent connection moves to the member that serves now", "remote", a.remote, "from", end.target, "to", serving)
				}
				return
			}
		}
	}
}

// flushAgent ends the control stream towards the agent and waits, briefly,
// for the agent to finish reading it. The line reader may hold the stream
// meanwhile; closing the connection afterwards ends both.
func (a *relayedAgent) flushAgent() {
	a.writeMu.Lock()
	if stream, ok := a.ctrl.(*yamux.Stream); ok {
		_ = stream.CloseWrite()
	}
	a.writeMu.Unlock()
	select {
	case <-a.done:
	case <-a.agent.CloseChan():
	case <-time.After(2 * time.Second):
	}
}

func (a *relayedAgent) writeAgent(line []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, err := a.ctrl.Write(line)
	return err
}

// memberLines passes the serving member's control lines to the agent, whole
// lines only: a session that ends mid-line leaves the agent's stream intact.
func (a *relayedAgent) memberLines(end *relayEnd, in *bufio.Reader) {
	for {
		line, err := relayLine(in)
		if err != nil {
			end.close()
			return
		}
		if a.writeAgent(line) != nil {
			a.close()
			return
		}
	}
}

// agentLines passes the agent's control lines to whichever member serves. A
// line is held while no member is attached, and sent again when one that was
// being written ends: the agent's stream is read no faster than it is passed.
func (a *relayedAgent) agentLines() {
	for {
		line, err := relayLine(a.in)
		if err != nil {
			a.close()
			return
		}
		for {
			a.mu.Lock()
			end, changed := a.current, a.changed
			a.mu.Unlock()
			if end != nil {
				if _, err := end.ctrl.Write(line); err == nil {
					break
				}
				end.close()
			}
			select {
			case <-a.done:
				return
			case <-changed:
			case <-time.After(relayAttachRetry):
			}
		}
	}
}

// agentStreams passes streams the agent opens (reports with a body, peer
// relays) to the serving member; with none attached the stream is refused.
func (a *relayedAgent) agentStreams() {
	for {
		stream, err := a.agent.AcceptStream()
		if err != nil {
			return
		}
		a.mu.Lock()
		end := a.current
		a.mu.Unlock()
		if end == nil {
			_ = stream.Reset()
			continue
		}
		go relaySplice(stream, func(ctx context.Context) (*yamux.Stream, error) { return end.sess.OpenStream(ctx) }, end.gone)
	}
}

// memberStreams passes streams the serving member opens to the agent.
func (a *relayedAgent) memberStreams(end *relayEnd) {
	for {
		stream, err := end.sess.AcceptStream()
		if err != nil {
			return
		}
		go relaySplice(stream, func(ctx context.Context) (*yamux.Stream, error) { return a.agent.OpenStream(ctx) }, end.gone)
	}
}

// relaySplice joins an accepted stream to one opened on the other side. Each
// direction's end of data is passed on as a half close; an error, or the end
// of the session the stream belongs to, resets both.
func relaySplice(from *yamux.Stream, open func(context.Context) (*yamux.Stream, error), gone <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), relayOpenTimeout)
	to, err := open(ctx)
	cancel()
	if err != nil {
		_ = from.Reset()
		return
	}
	finished := make(chan struct{})
	go func() {
		select {
		case <-gone:
			_ = from.Reset()
			_ = to.Reset()
		case <-finished:
		}
	}()
	var both sync.WaitGroup
	pass := func(dst, src *yamux.Stream) {
		defer both.Done()
		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Reset()
			_ = src.Reset()
			return
		}
		_ = dst.CloseWrite()
	}
	both.Add(2)
	go pass(to, from)
	go pass(from, to)
	both.Wait()
	close(finished)
	_ = from.Close()
	_ = to.Close()
}
