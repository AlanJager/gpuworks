package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	yamux "github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// relayMember is the control-plane end a relay attaches to: it speaks the
// server side of the agent handshake and records what reaches it.
type relayMember struct {
	t        *testing.T
	id       uint64
	reject   atomic.Bool
	mu       sync.Mutex
	attached int
	sessions []string
	remotes  []string
	hellos   []string
	lines    chan string
	sess     *yamux.Session
	ctrl     *yamux.Stream
	conn     net.Conn
}

func newRelayMember(t *testing.T, id uint64) *relayMember {
	return &relayMember{t: t, id: id, lines: make(chan string, 64)}
}

func (m *relayMember) serve(conn net.Conn, sessionID, remote string) {
	sess, err := yamux.Server(conn, laneYamuxConfig(proto.LaneCtrl, nil), nil)
	if err != nil {
		return
	}
	ctrl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	in := bufio.NewReader(ctrl)
	if _, err := in.ReadString('\n'); err != nil { // stream header
		return
	}
	hello, err := in.ReadString('\n')
	if err != nil {
		return
	}
	m.mu.Lock()
	m.attached++
	m.sessions, m.remotes, m.hellos = append(m.sessions, sessionID), append(m.remotes, remote), append(m.hellos, hello)
	m.sess, m.ctrl, m.conn = sess, ctrl, conn
	m.mu.Unlock()
	if m.reject.Load() {
		fmt.Fprintf(ctrl, `{"t":"reject","d":{"reason":"bad_token"}}`+"\n")
		_ = ctrl.CloseWrite()
		_, _ = io.Copy(io.Discard, ctrl) // as the hub does: let the peer read it
		_ = sess.Close()
		return
	}
	fmt.Fprintf(ctrl, `{"t":"registered","d":{"node_uuid":"n","session_id":%q,"member":%d}}`+"\n", sessionID, m.id)
	fmt.Fprintf(ctrl, `{"t":"hello","d":{"member":%d}}`+"\n", m.id)
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return
		}
		m.lines <- line
	}
}

func (m *relayMember) current() (*yamux.Session, *yamux.Stream, net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess, m.ctrl, m.conn
}

func (m *relayMember) attachments() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attached
}

func (m *relayMember) expect(want string) {
	m.t.Helper()
	select {
	case got := <-m.lines:
		if got != want {
			m.t.Fatalf("member %d received %q, want %q", m.id, got, want)
		}
	case <-time.After(10 * time.Second):
		m.t.Fatalf("member %d never received %q", m.id, want)
	}
}

// relayFixture is a relay in front of two members, and the leader the relay
// observes.
type relayFixture struct {
	t       *testing.T
	relay   *AgentRelay
	members map[uint64]*relayMember
	leader  atomic.Uint64
	server  *httptest.Server
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	f := &relayFixture{t: t, members: map[uint64]*relayMember{1: newRelayMember(t, 1), 2: newRelayMember(t, 2)}}
	attach := func(_ context.Context, _, sessionID, remote string) (net.Conn, uint64, bool, error) {
		leader := f.leader.Load()
		if leader == 0 {
			return nil, 0, false, nil
		}
		near, far := net.Pipe()
		go f.members[leader].serve(far, sessionID, remote)
		return near, leader, true, nil
	}
	target := func() (uint64, bool) { leader := f.leader.Load(); return leader, leader != 0 }
	f.relay = NewAgentRelay(nil, attach, target)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.relay.ServeWS(w, r, "198.51.100.7:4242") }))
	t.Cleanup(func() { f.relay.Close(); f.server.Close() })
	return f
}

// relayAgent is the agent's side of one connection through the relay.
type relayAgent struct {
	t    *testing.T
	sess *yamux.Session
	ctrl *yamux.Stream
	in   *bufio.Reader
}

func (f *relayFixture) dial(hello string) *relayAgent {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket.Dial owns handshake bodies; NetConn below owns the upgraded connection.
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/api/v1/agent/connect?lane=ctrl", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	conn := wsconn.Wrap(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	sess, err := yamux.Client(conn, laneYamuxConfig(proto.LaneCtrl, nil), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = sess.Close() })
	ctrl, err := sess.OpenStream(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	fmt.Fprint(ctrl, `{"kind":"ctrl"}`+"\n"+hello+"\n")
	return &relayAgent{t: f.t, sess: sess, ctrl: ctrl, in: bufio.NewReader(ctrl)}
}

func (a *relayAgent) read() (string, error) {
	_ = a.ctrl.SetReadDeadline(time.Now().Add(10 * time.Second))
	return a.in.ReadString('\n')
}

func (a *relayAgent) expectType(want string) proto.Envelope {
	a.t.Helper()
	line, err := a.read()
	if err != nil {
		a.t.Fatalf("agent expected %q: %v", want, err)
	}
	var env proto.Envelope
	if json.Unmarshal([]byte(line), &env) != nil || env.T != want {
		a.t.Fatalf("agent received %q, want a %q line", line, want)
	}
	return env
}

const relayHello = `{"t":"hello","d":{"node_token":"tok","proto_min":1,"proto_max":9}}`

func TestAgentRelayKeepsTheAgentConnectedAcrossAChangeOfServingMember(t *testing.T) {
	f := newRelayFixture(t)
	f.leader.Store(1)
	agent := f.dial(relayHello)
	registered := agent.expectType(proto.TRegistered)
	var reg proto.Registered
	_ = json.Unmarshal(registered.D, &reg)
	agent.expectType(proto.THello)
	one, two := f.members[1], f.members[2]
	if reg.SessionID == "" || one.sessions[0] != reg.SessionID || one.remotes[0] != "198.51.100.7:4242" || one.hellos[0] != relayHello+"\n" {
		t.Fatalf("first member saw session=%q remote=%q hello=%q; agent was told %q", one.sessions[0], one.remotes[0], one.hellos[0], reg.SessionID)
	}

	// Control lines pass both ways.
	fmt.Fprintf(agent.ctrl, `{"t":"status","d":1}`+"\n")
	one.expect(`{"t":"status","d":1}` + "\n")
	_, memberCtrl, _ := one.current()
	fmt.Fprintf(memberCtrl, `{"t":"state","d":"from-1"}`+"\n")
	if line, err := agent.read(); err != nil || line != `{"t":"state","d":"from-1"}`+"\n" {
		t.Fatalf("agent read %q %v", line, err)
	}
	// A stream the member opens reaches the agent, both directions and the
	// half close.
	memberSess, _, _ := one.current()
	done := make(chan string, 1)
	go func() {
		stream, err := agent.sess.AcceptStream()
		if err != nil {
			done <- err.Error()
			return
		}
		body, _ := io.ReadAll(stream)
		fmt.Fprintf(stream, "echo:%s", body)
		_ = stream.Close()
		done <- ""
	}()
	stream, err := memberSess.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(stream, "tunnel-bytes")
	_ = stream.CloseWrite()
	if got, err := io.ReadAll(stream); err != nil || string(got) != "echo:tunnel-bytes" {
		t.Fatalf("stream through the relay: %q %v", got, err)
	}
	if msg := <-done; msg != "" {
		t.Fatal(msg)
	}

	// Another member serves. The relay repeats the agent's hello to it with
	// the same session identity; the agent sees neither a second
	// registration nor the end of its connection.
	f.leader.Store(2)
	deadline := time.Now().Add(10 * time.Second)
	for two.attachments() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the relay never attached to the member that serves now")
		}
		time.Sleep(10 * time.Millisecond)
	}
	two.mu.Lock()
	session, hello := two.sessions[0], two.hellos[0]
	two.mu.Unlock()
	if session != reg.SessionID || hello != relayHello+"\n" {
		t.Fatalf("second member saw session=%q hello=%q", session, hello)
	}
	fmt.Fprintf(agent.ctrl, `{"t":"status","d":2}`+"\n")
	two.expect(`{"t":"status","d":2}` + "\n")
	for {
		if _, ctrl, _ := two.current(); ctrl != nil {
			fmt.Fprintf(ctrl, `{"t":"state","d":"from-2"}`+"\n")
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line, err := agent.read(); err != nil || line != `{"t":"state","d":"from-2"}`+"\n" {
		t.Fatalf("after the change the agent read %q %v (a repeated registration must not reach it)", line, err)
	}
	if agent.sess.IsClosed() || f.relay.Count() != 1 {
		t.Fatalf("agent connection closed=%t relayed=%d", agent.sess.IsClosed(), f.relay.Count())
	}
}

func TestAgentRelayHoldsLinesWhileNoMemberServesAndSurvivesItsDeath(t *testing.T) {
	f := newRelayFixture(t)
	f.leader.Store(1)
	agent := f.dial(relayHello)
	agent.expectType(proto.TRegistered)
	agent.expectType(proto.THello)
	one, two := f.members[1], f.members[2]

	// The serving member dies with a stream open through the relay: the
	// stream is reset, the agent's connection stays.
	memberSess, _, memberConn := one.current()
	accepted := make(chan *yamux.Stream, 1)
	go func() {
		stream, _ := agent.sess.AcceptStream()
		accepted <- stream
	}()
	stream, err := memberSess.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(stream, "x")
	agentStream := <-accepted
	f.leader.Store(0)
	_ = memberConn.Close()
	_ = agentStream.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(agentStream); err == nil {
		t.Fatal("a stream through a dead member ended as if it were complete")
	}
	// Nobody serves: a line the agent sends is held, not lost.
	fmt.Fprintf(agent.ctrl, `{"t":"status","d":"held"}`+"\n")
	time.Sleep(3 * relayAttachRetry)
	if agent.sess.IsClosed() {
		t.Fatal("the agent connection ended while no member served")
	}
	f.leader.Store(2)
	two.expect(`{"t":"status","d":"held"}` + "\n")
	if agent.sess.IsClosed() {
		t.Fatal("the agent connection ended across the gap")
	}
}

func TestAgentRelayEndsTheConnectionAMemberRefuses(t *testing.T) {
	f := newRelayFixture(t)
	// Refused at first: the agent reads the member's reject, then the end.
	f.members[1].reject.Store(true)
	f.leader.Store(1)
	agent := f.dial(relayHello)
	agent.expectType(proto.TReject)
	if _, err := agent.read(); err == nil {
		t.Fatal("a rejected agent connection stayed open")
	}
	// Refused by a later member (its token was regenerated meanwhile): the
	// reject reaches the agent and its connection ends.
	f.members[1].reject.Store(false)
	second := f.dial(relayHello)
	second.expectType(proto.TRegistered)
	second.expectType(proto.THello)
	f.members[2].reject.Store(true)
	f.leader.Store(2)
	second.expectType(proto.TReject)
	if _, err := second.read(); err == nil {
		t.Fatal("an agent a later member refused stayed connected")
	}
}

func TestAgentRelayPathCheckIsNotRepeatedAndCloseEndsEverything(t *testing.T) {
	f := newRelayFixture(t)
	f.leader.Store(1)
	// A handshake that only checks the path is relayed to one member once.
	smoke := f.dial(`{"t":"hello","d":{"node_token":"tok","smoke":true}}`)
	smoke.expectType(proto.TRegistered)
	smoke.expectType(proto.THello)
	_, _, conn := f.members[1].current()
	_ = conn.Close()
	if _, err := smoke.read(); err == nil {
		t.Fatal("a path check stayed open after its member ended it")
	}
	time.Sleep(3 * relayAttachRetry)
	if n := f.members[1].attachments(); n != 1 {
		t.Fatalf("a path check was relayed %d times", n)
	}
	// The data lane is not the relay's.
	resp, err := http.Get(f.server.URL + "/api/v1/agent/connect?lane=data")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("data lane on the relay answered %d", resp.StatusCode)
	}
	agent := f.dial(relayHello)
	agent.expectType(proto.TRegistered)
	agent.expectType(proto.THello)
	f.relay.Close()
	if _, err := agent.read(); err == nil {
		t.Fatal("a closed relay left an agent connected")
	}
	if f.relay.Count() != 0 {
		t.Fatalf("closed relay still holds %d connections", f.relay.Count())
	}
	resp, err = http.Get(f.server.URL + "/api/v1/agent/connect?lane=ctrl")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("closed relay answered %d", resp.StatusCode)
	}
}
