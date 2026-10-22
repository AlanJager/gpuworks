package outbound

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestSOCKSPinsAddressesRetainsAuthorityAndReusesConnection(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, authenticated := range []bool{false, true} {
			t.Run(fmt.Sprintf("TLS=%v/auth=%v", secure, authenticated), func(t *testing.T) {
				var requests atomic.Int32
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if !strings.HasPrefix(r.Host, "example.com:") || (secure && r.TLS.ServerName != "example.com") {
						t.Errorf("authority was lost: Host=%s TLS=%+v", r.Host, r.TLS)
					}
					w.WriteHeader(http.StatusNoContent)
				})
				origin := httptest.NewUnstartedServer(handler)
				if secure {
					origin.StartTLS()
				} else {
					origin.Start()
				}
				defer origin.Close()
				parsed, _ := url.Parse(origin.URL)
				proxy, attempts := socksFixture(t, parsed.Host, authenticated)
				transport := Transport(func() (proto.NetworkConfig, error) {
					return proto.NetworkConfig{HTTPProxy: proxy, HTTPSProxy: proxy}, nil
				}, "example.com")
				defer transport.CloseIdleConnections()
				if secure {
					transport.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				}
				var lookups atomic.Int32
				transport.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
					lookups.Add(1)
					return []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")}, nil
				}
				client := &http.Client{Transport: transport}
				for range 2 {
					response, err := client.Get(parsed.Scheme + "://example.com:" + parsed.Port())
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
				}
				if requests.Load() != 2 || attempts.Load() != 2 || lookups.Load() != 2 {
					t.Fatalf("origin requests=%d SOCKS attempts=%d DNS checks=%d", requests.Load(), attempts.Load(), lookups.Load())
				}
			})
		}
	}
}

func socksFixture(t *testing.T, origin string, authenticated bool) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var attempts atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var greeting [2]byte
				if _, err := io.ReadFull(conn, greeting[:]); err != nil {
					t.Error(err)
					return
				}
				methods := make([]byte, int(greeting[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					t.Error(err)
					return
				}
				if authenticated {
					_, _ = conn.Write([]byte{5, 2})
					var header [2]byte
					_, _ = io.ReadFull(conn, header[:])
					username := make([]byte, int(header[1]))
					_, _ = io.ReadFull(conn, username)
					var size [1]byte
					_, _ = io.ReadFull(conn, size[:])
					password := make([]byte, int(size[0]))
					_, _ = io.ReadFull(conn, password)
					if header[0] != 1 || string(username) != "review-user" || string(password) != "review-password" {
						t.Error("wrong SOCKS credentials")
						_, _ = conn.Write([]byte{1, 1})
						return
					}
					_, _ = conn.Write([]byte{1, 0})
				} else {
					_, _ = conn.Write([]byte{5, 0})
				}
				var request [4]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil {
					t.Error(err)
					return
				}
				if request != [4]byte{5, 1, 0, 1} {
					t.Errorf("SOCKS target was not a pinned IPv4 address: %v", request)
					return
				}
				var address [6]byte
				_, _ = io.ReadFull(conn, address[:])
				attempts.Add(1)
				if net.IP(address[:4]).String() == "127.0.0.2" {
					_, _ = conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				if net.IP(address[:4]).String() != "127.0.0.1" || fmt.Sprint(binary.BigEndian.Uint16(address[4:])) != strings.Split(origin, ":")[1] {
					t.Errorf("unexpected pinned target %v", address)
					return
				}
				upstream, err := net.Dial("tcp", origin)
				if err != nil {
					t.Error(err)
					return
				}
				defer upstream.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, conn); upstream.Close(); close(done) }()
				_, _ = io.Copy(conn, upstream)
				conn.Close()
				<-done
			}()
		}
	}()
	proxy := "socks5://" + listener.Addr().String()
	if authenticated {
		proxy = "socks5h://review-user:review-password@" + listener.Addr().String()
	}
	return proxy, &attempts
}

func TestSOCKSCancellationClosesPendingHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connected, closed := make(chan struct{}), make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(connected)
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}()
	proxy, _ := url.Parse("socks5://" + listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := dialSOCKSTunnel(ctx, proxy, "8.8.8.8:80"); result <- err }()
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS proxy did not accept connection")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil || (!errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled)) {
			t.Fatalf("unexpected cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS handshake ignored cancellation")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled SOCKS socket was left open")
	}
}
