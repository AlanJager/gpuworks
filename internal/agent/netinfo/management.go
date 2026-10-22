package netinfo

import (
	"net"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Management picks the node's management address: the local address of the
// connection to the server, which is by definition the one that reaches it.
// That address says nothing when the connection is to 127.0.0.1 (agent and
// server on one machine, or server_dial through an SSH tunnel) or goes
// through an HTTP proxy; the default route's address stands in then. The
// interface is the inventory entry holding the address.
func Management(local net.IP, viaProxy bool, inv proto.NetInventory, defaultRoute func() (net.IP, error)) *proto.ManagementAddress {
	ip := local
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || viaProxy {
		ip = nil
		if defaultRoute != nil {
			if routed, err := defaultRoute(); err == nil && routed != nil && !routed.IsLoopback() && !routed.IsUnspecified() {
				ip = routed
			}
		}
	}
	if ip != nil && fakeIPRange.Contains(ip) {
		// A proxy's TUN mode (Clash, Surge, sing-box) routes every connection,
		// the server's and the default route alike, through an interface whose
		// address no other node can reach. A NIC's own address stands in.
		ip = nicAddress(inv)
	}
	if ip == nil {
		return nil
	}
	out := &proto.ManagementAddress{IP: ip.String()}
	for _, iface := range inv.Interfaces {
		for _, cidr := range iface.Addrs {
			addr, _, err := net.ParseCIDR(cidr)
			if err != nil {
				addr = net.ParseIP(strings.TrimSpace(cidr))
			}
			if addr != nil && addr.Equal(ip) {
				out.Interface = iface.Name
				return out
			}
		}
	}
	return out
}

// fakeIPRange is 198.18.0.0/15, reserved for network benchmarking (RFC 2544)
// and so never a real network; proxies hand it out as fake IPs.
var fakeIPRange = &net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)}

// nicAddress is the first IPv4 address a reported interface holds outside
// loopback, link-local and the fake-IP range, taking interfaces with a MAC
// (hardware) before tunnels, which have none, and otherwise by name.
func nicAddress(inv proto.NetInventory) net.IP {
	for _, hardware := range []bool{true, false} {
		for _, iface := range inv.Interfaces {
			if (iface.MAC != "") != hardware {
				continue
			}
			for _, cidr := range iface.Addrs {
				addr, _, err := net.ParseCIDR(cidr)
				if err != nil || addr.To4() == nil || addr.IsLoopback() || addr.IsLinkLocalUnicast() || fakeIPRange.Contains(addr) {
					continue
				}
				return addr
			}
		}
	}
	return nil
}

// DefaultRouteIP is the local address a packet to host would leave from;
// dialing UDP sends nothing. A loopback or empty host asks about a public
// address instead, which is the default route.
func DefaultRouteIP(host string) func() (net.IP, error) {
	return func() (net.IP, error) {
		target := "223.5.5.5"
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			target = ip.String()
		} else if ip == nil && host != "" && host != "localhost" {
			if addrs, err := net.LookupIP(host); err == nil && len(addrs) > 0 && !addrs[0].IsLoopback() {
				target = addrs[0].String()
			}
		}
		conn, err := net.Dial("udp", net.JoinHostPort(target, "53"))
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP, nil
	}
}
