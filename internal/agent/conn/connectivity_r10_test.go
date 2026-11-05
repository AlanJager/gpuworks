package conn

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestConnectivityR10PendingThenAttachedThenDetached(t *testing.T) {
	peer := &laneTestServer{sessionID: "runtime-private-session"}
	firstPermit, firstRequest, secondRequest := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	var permitOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("lane") == proto.LaneData {
			if requests.Add(1) == 1 {
				close(firstRequest)
				select {
				case <-firstPermit:
				case <-r.Context().Done():
					return
				}
			} else {
				select {
				case <-secondRequest:
				default:
					close(secondRequest)
				}
				<-r.Context().Done()
				return
			}
		}
		peer.handle(w, r)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	c := New(Credentials{ServerURL: server.URL, NodeToken: "private-node", DataLane: true}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		permitOnce.Do(func() { close(firstPermit) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned control did not close")
		}
		server.Close()
	}()
	select {
	case <-firstRequest:
	case <-time.After(3 * time.Second):
		t.Fatal("actual data request never started")
	}
	if got := proto.ConnectionValue(c.ConnectionReport(), "connection.data_lane"); got != "pending" {
		t.Fatalf("negotiated but never attached data=%s", got)
	}
	permitOnce.Do(func() { close(firstPermit) })
	deadline := time.Now().Add(3 * time.Second)
	for proto.ConnectionValue(c.ConnectionReport(), "connection.data_lane") != "attached" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := proto.ConnectionValue(c.ConnectionReport(), "connection.data_lane"); got != "attached" {
		t.Fatalf("actual data attach not observed: %s", got)
	}
	c.dataMu.Lock()
	session := c.dataSess
	c.dataMu.Unlock()
	_ = session.Close()
	select {
	case <-secondRequest:
	case <-time.After(3 * time.Second):
		t.Fatal("normal data retry never started")
	}
	if got := proto.ConnectionValue(c.ConnectionReport(), "connection.data_lane"); got != "detached" {
		t.Fatalf("lost previously attached data=%s", got)
	}
}

func TestConnectivityR10ProxyCertificateIsNotTargetTLS(t *testing.T) {
	for _, target := range []string{"http://target.invalid", "https://target.invalid"} {
		t.Run(target, func(t *testing.T) {
			proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
			defer proxy.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			connectivityTransport(t, func(transport *http.Transport) {
				transport.Proxy = http.ProxyURL(proxyURL)
				transport.TLSClientConfig = nil
			})
			report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: target, NodeToken: "private-node"}, nil)
			expected := "not_observed"
			if target[:5] == "http:" {
				expected = "not_applicable"
			}
			if err == nil || proto.ConnectionValue(report, "connection.reason") != "tls" || proto.ConnectionValue(report, "connection.phase.tls") != expected || proto.ConnectionValue(report, "connection.phase.connect") != "ok" {
				t.Fatalf("proxy certificate failure rewrote target/TCP evidence: TLS=%s connect=%s reason=%s", proto.ConnectionValue(report, "connection.phase.tls"), proto.ConnectionValue(report, "connection.phase.connect"), proto.ConnectionValue(report, "connection.reason"))
			}
		})
	}
}

func TestConnectivityR10ReturnedTokenRevalidatesSafeURLs(t *testing.T) {
	for _, path := range []string{"/%73ecret", "/private-node", "/private-node/%73ecret"} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc((&laneTestServer{}).handle))
			defer server.Close()
			// The real Registered fixture returns NodeToken=secret, distinct from input.
			report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL + path, NodeToken: "private-node"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if proto.ConnectionValue(report, "connection.server_url") != "" {
				t.Fatal("a current or original credential survived URL reprojection")
			}
		})
	}
	t.Run("proxy-origin", func(t *testing.T) {
		c := New(Credentials{ServerURL: "https://node.example/private-node", NodeToken: "private-node", Smoke: true}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		c.observationFact("proxy_url", "https://secret.proxy.invalid")
		c.creds.NodeToken = "secret"
		c.connectionReady()
		report := c.ConnectionReport()
		if proto.ConnectionValue(report, "connection.proxy_url") != "" || proto.ConnectionValue(report, "connection.server_url") != "" {
			t.Fatal("returned token or previously hidden original token was exposed")
		}
	})
}

func TestConnectivityR10AcceptedAuthFailurePreservesOutboundNA(t *testing.T) {
	peer, accepted := net.Pipe()
	_ = peer.Close()
	c := New(Credentials{NodeToken: "private-node", ConnectMode: "server_dial"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.RunAccepted(context.Background(), accepted); err == nil {
		t.Fatal("expected pre-auth peer close")
	}
	report := c.ConnectionReport()
	for _, stage := range []string{"dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade"} {
		if got := proto.ConnectionValue(report, "connection.phase."+stage); got != "not_applicable" {
			t.Fatalf("inbound auth rewrote outbound %s=%s", stage, got)
		}
	}
	if proto.ConnectionValue(report, "connection.outcome") != "failed" {
		t.Fatal("inbound failure was lost")
	}
}

func TestConnectivityR10TargetTLSFailureFailsAggregateDial(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc((&laneTestServer{}).handle))
	defer server.Close()
	connectivityTransport(t, func(transport *http.Transport) { transport.Proxy = nil; transport.TLSClientConfig = nil })
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "private-node"}, nil)
	if err == nil || proto.ConnectionValue(report, "connection.phase.tls") != "failed" || proto.ConnectionValue(report, "connection.phase.dial") != "failed" {
		t.Fatalf("failed target TLS left successful aggregate dial: TLS=%s dial=%s", proto.ConnectionValue(report, "connection.phase.tls"), proto.ConnectionValue(report, "connection.phase.dial"))
	}
}
