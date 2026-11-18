package hub

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStopSessionsCancelsUnfinishedWebSocketHandshakes(t *testing.T) {
	for _, lane := range []string{"", "ctrl", "data"} {
		t.Run("lane="+lane, func(t *testing.T) {
			h := newTestHub(t)
			t.Cleanup(func() { _ = h.store.Close() })
			t.Cleanup(h.StopSessions)
			_, token := newTestNode(t, h.store)
			server := httptest.NewServer(http.HandlerFunc(h.ServeWS))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/?lane="+lane, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.CloseNow()
			// No yamux/hello is sent. The connection must already be owned.
			done := make(chan struct{})
			go func() { h.StopSessions(); close(done) }()
			for {
				// yamux may have already queued a frame before cancellation;
				// consume those frames until the transport actually closes.
				if _, _, err := ws.Read(ctx); err != nil {
					if ctx.Err() != nil {
						t.Fatalf("pending handshake did not close promptly: %v", err)
					}
					break
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("StopSessions did not wait for the handshake to finish")
			}
			response, err := http.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("admission after stop=%d", response.StatusCode)
			}
			h.StartSessions()
			c := agentconn.New(agentconn.Credentials{ServerURL: server.URL, NodeToken: token, Smoke: true}, nil, nil, emptyProber{}, h.log)
			if err := c.Run(ctx); err != nil {
				t.Fatalf("fresh epoch could not authenticate unchanged token: %v", err)
			}
		})
	}
}

func TestStopSessionsCancelsServerDialBeforeAuthentication(t *testing.T) {
	h := newTestHub(t)
	t.Cleanup(func() { _ = h.store.Close() })
	t.Cleanup(h.StopSessions)
	uuid, token := newTestNode(t, h.store)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	dialDone := make(chan error, 1)
	go func() { dialDone <- h.DialAgentWithSecret(context.Background(), listener.Addr().String(), token) }()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("server_dial did not connect")
	}
	defer peer.Close()
	// The peer withholds its challenge. No successful Session exists to close.
	h.StopSessions()
	select {
	case err := <-dialDone:
		if err == nil {
			t.Fatal("unfinished authentication succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("server_dial handshake outlived StopSessions")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.Copy(io.Discard, peer); err != nil {
		t.Fatalf("cancelled handshake socket did not close: %v", err)
	}
	if h.Online(uuid) {
		t.Fatal("unauthenticated node became online")
	}
	if err := h.DialAgentWithSecret(context.Background(), listener.Addr().String(), token); !errors.Is(err, errSessionsStopped) {
		t.Fatalf("dial admitted after stop: %v", err)
	}
}

func TestStopSessionsWaitsForTelemetryAndSerializesTheNextEpoch(t *testing.T) {
	h := newTestHub(t)
	t.Cleanup(func() { _ = h.store.Close() })
	t.Cleanup(h.StopSessions)
	g := newGatedIngest(h)
	var release sync.Once
	defer release.Do(func() { close(g.release) })
	_, token := newTestNode(t, h.store)
	server := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(server.Close)
	_, reg := dialCtrlLane(t, server, token)
	data := dialDataLane(t, server, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})
	stream := openAgentStream(t, data, proto.StreamTelemetry)
	if err := writeEnvelopeLine(stream, proto.TLogs, proto.LogBatch{Lines: []proto.LogLine{{Line: "last batch"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("telemetry did not reach the sink")
	}
	stopped := make(chan struct{})
	go func() { h.StopSessions(); close(stopped) }()
	// Observe the public gate closed before starting the next epoch. This
	// avoids a timing-only assertion that Stop merely has not been scheduled.
	waitFor(t, 3*time.Second, func() bool {
		response, err := http.Get(server.URL)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusServiceUnavailable
	}, "session admission to close")
	restarted := make(chan struct{})
	go func() { h.StartSessions(); close(restarted) }()
	select {
	case <-stopped:
		t.Fatal("stopped under an in-flight telemetry write")
	default:
	}
	select {
	case <-restarted:
		t.Fatal("new epoch started before old telemetry drained")
	default:
	}
	release.Do(func() { close(g.release) })
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("StopSessions did not finish after telemetry drained")
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("next epoch did not reopen")
	}
	if got := g.waitLines(t, 1); len(got) != 1 || got[0] != "last batch" {
		t.Fatalf("last telemetry=%v", got)
	}
}
