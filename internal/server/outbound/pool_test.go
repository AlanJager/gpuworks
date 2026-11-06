package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestProxyRetriesValidatedAddressesBeforeSendingRequest(t *testing.T) {
	var requests, attempts atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	parsed, _ := url.Parse(origin.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Host == net.JoinHostPort("127.0.0.2", parsed.Port()) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Method != http.MethodConnect || r.Host != parsed.Host {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		upstream, err := net.Dial("tcp", parsed.Host)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, conn); upstream.Close(); close(done) }()
		_, _ = io.Copy(conn, upstream)
		conn.Close()
		<-done
	}))
	defer proxy.Close()
	transport := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{HTTPProxy: proxy.URL}, nil }, "mirror.corp")
	defer transport.CloseIdleConnections()
	transport.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	response, err := (&http.Client{Transport: transport}).Post("http://mirror.corp:"+parsed.Port()+"/", "text/plain", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if attempts.Load() != 2 || requests.Load() != 1 {
		t.Fatalf("CONNECT attempts=%d origin requests=%d", attempts.Load(), requests.Load())
	}
}

func TestPooledConnectionStillRequiresFreshSafeDNS(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	parsed, _ := url.Parse(origin.URL)
	transport := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, "mirror.corp")
	defer transport.CloseIdleConnections()
	var lookups atomic.Int32
	transport.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	client := &http.Client{Transport: transport}
	response, err := client.Get("http://mirror.corp:" + parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err := client.Get("http://mirror.corp:" + parsed.Port()); !errors.Is(err, ErrTargetDenied) {
		t.Fatalf("second DNS answer was not rejected: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("origin received %d requests", requests.Load())
	}
}

func TestProxyPoolConfigurationsAreBounded(t *testing.T) {
	transport := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil })
	defer transport.CloseIdleConnections()
	for i := range maxTransportPools * 3 {
		proxy, _ := url.Parse(fmt.Sprintf("http://proxy.example:%d", 1000+i))
		transport.requestTransport(proxy, pinnedTarget{})
	}
	if len(transport.transports) != maxTransportPools || len(transport.transportOrder) != maxTransportPools {
		t.Fatalf("unbounded proxy cache: transports=%d order=%d", len(transport.transports), len(transport.transportOrder))
	}
}
