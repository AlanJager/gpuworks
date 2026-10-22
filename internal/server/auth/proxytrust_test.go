package auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestNewProxyTrustParsesAddressesAndCIDRs(t *testing.T) {
	p, invalid := NewProxyTrust(" 10.0.0.0/8, 192.0.2.10 ,::1,, fd00::/8, nonsense, 300.1.1.1", nil)
	if want := []string{"nonsense", "300.1.1.1"}; !reflect.DeepEqual(invalid, want) {
		t.Fatalf("invalid = %v, want %v", invalid, want)
	}
	for addr, want := range map[string]bool{
		"10.1.2.3:443":          true,
		"10.1.2.3":              true,
		"192.0.2.10:1234":       true,
		"192.0.2.11:1234":       false,
		"[::1]:80":              true,
		"[fd12::1]:80":          true,
		"[::ffff:10.1.2.3]:443": true,
		"198.51.100.7:443":      false,
		"not-an-ip:80":          false,
		"":                      false,
	} {
		if got := p.Listed(addr); got != want {
			t.Errorf("Listed(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestProxyTrustPeer(t *testing.T) {
	enabled := true
	exposed, _ := NewProxyTrust("10.0.0.0/8", func() bool { return enabled })
	listedOnly, _ := NewProxyTrust("10.0.0.0/8", nil)
	request := func(peer, local string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		if local != "" {
			addr, err := net.ResolveTCPAddr("tcp", local)
			if err != nil {
				t.Fatal(err)
			}
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, addr))
		}
		return r
	}
	for _, tc := range []struct {
		name        string
		p           *ProxyTrust
		peer, local string
		enabled     bool
		want        bool
	}{
		{"nil trusts nobody", nil, "10.1.2.3:1", "", true, false},
		{"listed", listedOnly, "10.1.2.3:1", "", false, true},
		{"loopback without same-host rule", listedOnly, "127.0.0.1:1", "127.0.0.1:8800", true, false},
		{"loopback while exposed", exposed, "127.0.0.1:1", "", true, true},
		{"IPv6 loopback while exposed", exposed, "[::1]:1", "[::1]:8800", true, true},
		{"own address while exposed", exposed, "172.25.17.156:1", "172.25.17.156:8800", true, true},
		{"own address, entry point off", exposed, "172.25.17.156:1", "172.25.17.156:8800", false, false},
		{"loopback, entry point off", exposed, "[::1]:1", "[::1]:8800", false, false},
		{"another host while exposed", exposed, "172.25.17.200:1", "172.25.17.156:8800", true, false},
		{"no local address known", exposed, "172.25.17.156:1", "", true, false},
		{"unparsable peer", exposed, "garbage", "172.25.17.156:8800", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled = tc.enabled
			if got := tc.p.Peer(request(tc.peer, tc.local)); got != tc.want {
				t.Fatalf("Peer = %v, want %v", got, tc.want)
			}
		})
	}
}

// The entry point's state is a file read: a peer from elsewhere must not cost
// one, since every agent connection asks.
func TestProxyTrustAsksSameHostOnlyForLocalPeers(t *testing.T) {
	calls := 0
	p, _ := NewProxyTrust("10.0.0.0/8", func() bool { calls++; return true })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, peer := range []string{"10.1.2.3:1", "198.51.100.7:1"} {
		r.RemoteAddr = peer
		p.Peer(r)
	}
	if calls != 0 {
		t.Fatalf("sameHost asked %d times for listed or remote peers", calls)
	}
}
