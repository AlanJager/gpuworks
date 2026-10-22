package hub

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// laneStreamFixture is a session with an attached data lane and a tunnel
// stream already open on it, plus the agent's end of that stream with the
// stream header consumed — the state a proxied request is in while it waits
// for the engine's response.
type laneStreamFixture struct {
	sess       *Session
	data       *yamux.Session
	dataClient *yamux.Session
	conn       net.Conn
	agent      *yamux.Stream
	agentRead  *bufio.Reader
}

func newLaneStreamFixture(t *testing.T) laneStreamFixture {
	t.Helper()
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)
	data, dataClient := yamuxPairData(t)
	sess.attachData(data)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := sess.openStream(ctx, proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "127.0.0.1:30000"})
	if err != nil {
		t.Fatalf("open tunnel on the attached data lane: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	agent, err := dataClient.AcceptStream()
	if err != nil {
		t.Fatalf("agent never received the stream: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	agentRead := bufio.NewReader(agent)
	if _, err := agentRead.ReadBytes('\n'); err != nil {
		t.Fatalf("read stream header: %v", err)
	}
	return laneStreamFixture{sess: sess, data: data, dataClient: dataClient, conn: conn, agent: agent, agentRead: agentRead}
}

// readErr reads from conn until it fails, bounded so a regression that makes
// the read hang fails the test instead of the suite.
func readErr(t *testing.T, conn net.Conn) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, conn)
		if err == nil {
			err = io.EOF // io.Copy reports a clean end as nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("read on the tunnel stream did not return")
		return nil
	}
}

// T56: the request's stream is open and waiting for the engine's response when
// the data lane dies. The read must fail as the typed, retryable
// ErrDataLaneUnavailable (the proxy answers 503), not as a raw stream reset
// the proxy can only map to 502; the dead lane is detached at once, and a
// write after the death is classified the same way.
func TestLaneStream_LaneDeathUnderAnOpenStreamIsDataLaneUnavailable(t *testing.T) {
	f := newLaneStreamFixture(t)
	if _, ok := f.conn.(*laneStream); !ok {
		t.Fatalf("a data-lane stream is %T, want *laneStream", f.conn)
	}
	if _, err := f.conn.Write([]byte("GET /v1/bulk HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("request write on a live lane: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- readErr(t, f.conn) }()
	time.Sleep(50 * time.Millisecond) // the read is parked waiting for a response
	_ = f.dataClient.Close()          // the lane's transport dies under it

	err := <-result
	if !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("read after the lane died = %v, want ErrDataLaneUnavailable", err)
	}
	if !errors.Is(err, yamux.ErrStreamReset) {
		t.Fatalf("read error %v lost the underlying stream reset", err)
	}
	if got := f.sess.DataLaneState(); got != "detached" {
		t.Fatalf("lane state after the death = %q, want detached", got)
	}
	if _, err := f.conn.Write([]byte("more")); !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("write after the lane died = %v, want ErrDataLaneUnavailable", err)
	}
}

// The agent resetting one stream (its dial to the engine failed, or the engine
// dropped the connection) is an upstream failure on a healthy lane. It must
// keep its own error — the proxy answers it as 502 — and must not detach or
// close the lane every other stream is using.
func TestLaneStream_AgentResetOnALiveLaneStaysAnUpstreamError(t *testing.T) {
	f := newLaneStreamFixture(t)
	if err := f.agent.Reset(); err != nil {
		t.Fatalf("agent reset: %v", err)
	}
	err := readErr(t, f.conn)
	if errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("a stream reset on a live lane was reported as the lane being unavailable: %v", err)
	}
	if !errors.Is(err, yamux.ErrStreamReset) {
		t.Fatalf("read after the agent reset = %v, want the stream reset", err)
	}
	if f.data.IsClosed() {
		t.Fatal("one reset stream closed the whole data lane")
	}
	if got := f.sess.DataLaneState(); got != "attached" {
		t.Fatalf("lane state after one stream reset = %q, want attached", got)
	}
}

// A response that ends normally is still io.EOF, and the stream keeps the
// half-close a caller type-asserts for.
func TestLaneStream_CleanEndAndHalfClosePassThrough(t *testing.T) {
	f := newLaneStreamFixture(t)
	cw, ok := f.conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T lost CloseWrite", f.conn)
	}
	if _, err := f.conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(f.agentRead)
	if err != nil || string(got) != "request" {
		t.Fatalf("agent read = %q, %v; want the request then EOF from the half-close", got, err)
	}
	if _, err := f.agent.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	_ = f.agent.Close()
	body, err := io.ReadAll(f.conn)
	if err != nil || string(body) != "response" {
		t.Fatalf("response read = %q, %v; want the body and a clean end", body, err)
	}
}

// A node that never attached a data lane carries data-plane kinds on ctrl;
// those streams are not data-lane streams and are returned as they are.
func TestLaneStream_CtrlCarriedStreamIsNotWrapped(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, ctrlClient := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)
	go func() {
		for {
			st, err := ctrlClient.AcceptStream()
			if err != nil {
				return
			}
			_ = st.Close()
		}
	}()
	conn, err := sess.openStream(context.Background(), proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "127.0.0.1:30000"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, wrapped := conn.(*laneStream); wrapped {
		t.Fatal("a stream carried by ctrl was wrapped as a data-lane stream")
	}
}
