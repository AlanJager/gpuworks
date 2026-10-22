package auth

import (
	"net"
	"net/http"
	"strings"
)

// ProxyTrust decides whose X-Forwarded-* headers the server believes: the
// proxies the operator listed, and, while sameHost says so, any connection
// made from this host. One value serves both the console's origin checks and
// the agent hub's source addresses, so the two never disagree about a proxy.
//
// sameHost is how the public entry point is trusted (ADR 0049): its connector
// always runs on the server's own host and forwards the browser's https
// origin, whichever local address it was pointed at.
type ProxyTrust struct {
	nets     []*net.IPNet
	sameHost func() bool
}

// NewProxyTrust parses comma-separated IPs and CIDRs and returns the entries it
// could not parse. sameHost may be nil: only the listed proxies are trusted.
func NewProxyTrust(list string, sameHost func() bool) (*ProxyTrust, []string) {
	p := &ProxyTrust{sameHost: sameHost}
	var invalid []string
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			bits := 128
			if v4 := ip.To4(); v4 != nil {
				ip, bits = v4, 32
			}
			p.nets = append(p.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			invalid = append(invalid, raw)
			continue
		}
		p.nets = append(p.nets, network)
	}
	return p, invalid
}

// Peer reports whether the peer that sent r may supply forwarded headers. A
// nil ProxyTrust trusts nobody.
func (p *ProxyTrust) Peer(r *http.Request) bool {
	if p == nil {
		return false
	}
	ip := hostIP(r.RemoteAddr)
	if ip == nil {
		return false
	}
	if p.contains(ip) {
		return true
	}
	return p.sameHost != nil && fromThisHost(r, ip) && p.sameHost()
}

// Listed reports whether addr, an IP or host:port, is one of the listed
// proxies. A forwarded chain skips these hops to find the client.
func (p *ProxyTrust) Listed(addr string) bool {
	if p == nil {
		return false
	}
	ip := hostIP(addr)
	return ip != nil && p.contains(ip)
}

func (p *ProxyTrust) contains(ip net.IP) bool {
	for _, network := range p.nets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// fromThisHost reports a connection made from this host: a loopback peer, or
// one whose address is the very address it reached. A connection to a local
// address leaves from that address, and the kernel drops a packet from outside
// that claims a local source, so a remote peer cannot pass for this host.
func fromThisHost(r *http.Request, peer net.IP) bool {
	if peer.IsLoopback() {
		return true
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	return ok && local.IP.Equal(peer)
}

func hostIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	return net.ParseIP(host)
}
