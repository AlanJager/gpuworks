package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type forwarderFixture struct {
	f     *servingForwarder
	token string
	mu    sync.Mutex
	ports map[string]int
}

func newForwarderFixture(t *testing.T) *forwarderFixture {
	t.Helper()
	fx := &forwarderFixture{token: "node-token-a", ports: map[string]int{}}
	fx.f = newServingForwarder(slog.Default(), func() string {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.token
	}, func(uuid string) (int, bool) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		p, ok := fx.ports[uuid]
		return p, ok
	})
	fx.f.listenPort = 0
	t.Cleanup(fx.f.close)
	return fx
}

func (fx *forwarderFixture) addr(t *testing.T) string {
	t.Helper()
	fx.f.mu.Lock()
	defer fx.f.mu.Unlock()
	if fx.f.listener == nil {
		t.Fatal("forwarder is not listening")
	}
	return fx.f.listener.Addr().String()
}

// dialForward opens a pinned TLS connection and sends the preamble,
// returning the connection, its reader and the agent's reply line.
func dialForward(t *testing.T, addr, pinToken, instance, token string) (*tls.Conn, *bufio.Reader, string, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, proto.ServingForwardClientTLS(pinToken))
	if err != nil {
		return nil, nil, "", err
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "%s %s %s\n", proto.ServingForwardMagic, instance, token)
	r := bufio.NewReader(conn)
	reply, err := r.ReadString('\n')
	return conn, r, reply, err
}

func upstreamPort(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return p
}

func TestServingForwarderSplicesAuthenticatedConnections(t *testing.T) {
	fx := newForwarderFixture(t)
	fx.ports["inst-1"] = upstreamPort(t, func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
		}
	})
	fx.f.apply("127.0.0.1")
	addr := fx.addr(t)

	conn, r, reply, err := dialForward(t, addr, fx.token, "inst-1", proto.ServingForwardToken(fx.token, "inst-1"))
	if err != nil || reply != "OK\n" {
		t.Fatalf("reply %q, %v", reply, err)
	}
	fmt.Fprintf(conn, "GET /v1/stream HTTP/1.1\r\nHost: engine\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(r)
	if !strings.Contains(string(body), "200 OK") || !strings.Contains(string(body), "data: 2") {
		t.Fatalf("response = %q", body)
	}

	for _, tc := range []struct {
		name, instance, token, want string
	}{
		{"wrong token", "inst-1", proto.ServingForwardToken(fx.token, "inst-2"), "ERR rejected authentication\n"},
		{"token of another node", "inst-1", proto.ServingForwardToken("other", "inst-1"), "ERR rejected authentication\n"},
		{"instance not here", "inst-9", proto.ServingForwardToken(fx.token, "inst-9"), "ERR instance is not running here\n"},
	} {
		if _, _, reply, err := dialForward(t, addr, fx.token, tc.instance, tc.token); err != nil || reply != tc.want {
			t.Fatalf("%s: reply %q, %v", tc.name, reply, err)
		}
	}

	raw, err := tls.Dial("tcp", addr, proto.ServingForwardClientTLS(fx.token))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	io.WriteString(raw, "GET / HTTP/1.1\r\n")
	if reply, _ := bufio.NewReader(raw).ReadString('\n'); reply != "ERR bad preamble\n" {
		t.Fatalf("bad preamble reply %q", reply)
	}
	long, err := tls.Dial("tcp", addr, proto.ServingForwardClientTLS(fx.token))
	if err != nil {
		t.Fatal(err)
	}
	defer long.Close()
	io.WriteString(long, strings.Repeat("x", 400))
	if reply, _ := bufio.NewReader(long).ReadString('\n'); reply != "ERR preamble too long\n" {
		t.Fatalf("long preamble reply %q", reply)
	}
}

func TestServingForwarderReportsAnUnreachableInstancePort(t *testing.T) {
	fx := newForwarderFixture(t)
	fx.ports["inst-1"] = 1
	fx.f.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }
	fx.f.apply("127.0.0.1")
	_, _, reply, err := dialForward(t, fx.addr(t), fx.token, "inst-1", proto.ServingForwardToken(fx.token, "inst-1"))
	if err != nil || reply != "ERR instance port: connection refused\n" {
		t.Fatalf("reply %q, %v", reply, err)
	}
}

func TestServingForwarderFollowsServingIPAndNodeToken(t *testing.T) {
	fx := newForwarderFixture(t)
	fx.f.apply("")
	if fx.f.listener != nil {
		t.Fatal("listening without a serving IP")
	}
	fx.f.apply("127.0.0.1")
	first := fx.addr(t)
	fx.f.apply("127.0.0.1") // unchanged: same listener
	if fx.addr(t) != first {
		t.Fatal("idempotent apply restarted the listener")
	}

	// A reissued node token rotates the TLS key: the old pin no longer works.
	fx.mu.Lock()
	fx.token = "node-token-b"
	fx.mu.Unlock()
	fx.f.apply("127.0.0.1")
	addr := fx.addr(t)
	if _, err := tls.Dial("tcp", addr, proto.ServingForwardClientTLS("node-token-a")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("old pin err = %v", err)
	}
	if _, _, reply, err := dialForward(t, addr, "node-token-b", "inst-x", proto.ServingForwardToken("node-token-b", "inst-x")); err != nil || reply != "ERR instance is not running here\n" {
		t.Fatalf("new pin reply %q, %v", reply, err)
	}

	fx.f.apply("")
	if fx.f.listener != nil {
		t.Fatal("still listening after the serving network was removed")
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("old listener still accepts")
	}
}

func TestServingForwarderSurvivesListenFailure(t *testing.T) {
	fx := newForwarderFixture(t)
	fx.f.listen = func(string, string) (net.Listener, error) { return nil, errors.New("address in use") }
	fx.f.apply("127.0.0.1")
	if fx.f.listener != nil || fx.f.addr != "" {
		t.Fatal("failed listen left state behind")
	}
	fx.f.listen = net.Listen
	fx.f.apply("127.0.0.1")
	fx.addr(t)
}

func TestNodeRuntimeWiresTheForwarderToStateAndToken(t *testing.T) {
	n := newTestNodeRuntimeForForwarder(t)
	n.servingForward.listenPort = 0
	n.SetNodeToken("tok")
	n.setNetwork(proto.NetworkConfig{ServingIP: "127.0.0.1"})
	if n.servingForward.listener == nil {
		t.Fatal("state sync with a serving IP did not start the forwarder")
	}
	if _, ok := n.instanceAPIPort("missing"); ok {
		t.Fatal("unknown instance resolved to a port")
	}
	n.setNetwork(proto.NetworkConfig{})
	if n.servingForward.listener != nil {
		t.Fatal("forwarder kept listening without a serving IP")
	}
}

func newTestNodeRuntimeForForwarder(t *testing.T) *NodeRuntime {
	t.Helper()
	n := &NodeRuntime{log: slog.Default()}
	n.servingForward = newServingForwarder(n.log, n.currentNodeToken, n.instanceAPIPort)
	t.Cleanup(n.servingForward.close)
	return n
}
