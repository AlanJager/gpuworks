package netinfo

import (
	"errors"
	"net"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestManagementUsesTheConnectionsLocalAddress(t *testing.T) {
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{
		{Name: "ib0", Addrs: []string{"10.96.0.5/13"}},
		{Name: "eth0", Addrs: []string{"fe80::1/64", "172.25.17.156/16"}},
	}}
	route := func() (net.IP, error) { return net.ParseIP("10.96.0.5"), nil }
	got := Management(net.ParseIP("172.25.17.156"), false, inv, route)
	if got == nil || got.IP != "172.25.17.156" || got.Interface != "eth0" {
		t.Fatalf("management = %+v", got)
	}
	// Loopback (same machine, SSH tunnel) and a proxy say nothing about the
	// node's own network; the default route stands in.
	for _, c := range []struct {
		local    net.IP
		viaProxy bool
	}{{net.ParseIP("127.0.0.1"), false}, {nil, false}, {net.ParseIP("172.25.17.156"), true}} {
		got := Management(c.local, c.viaProxy, inv, route)
		if got == nil || got.IP != "10.96.0.5" || got.Interface != "ib0" {
			t.Fatalf("fallback for %v proxy=%v = %+v", c.local, c.viaProxy, got)
		}
	}
	// An address no reported interface holds keeps the address alone.
	if got := Management(net.ParseIP("192.0.2.9"), false, inv, route); got == nil || got.IP != "192.0.2.9" || got.Interface != "" {
		t.Fatalf("unmatched = %+v", got)
	}
	noRoute := func() (net.IP, error) { return nil, errors.New("network is unreachable") }
	if got := Management(net.ParseIP("127.0.0.1"), false, inv, noRoute); got != nil {
		t.Fatalf("no usable address = %+v", got)
	}
}

// A MacBook whose proxy runs in TUN mode: the connection to the server and the
// default route both leave through utun with the proxy's fake IP, which other
// nodes cannot dial (a model sync from a peer timed out on 198.18.0.1:51439).
func TestManagementSkipsAProxysFakeIP(t *testing.T) {
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{
		{Name: "en0", MAC: "5c:1b:f4:7a:10:21", Addrs: []string{"fe80::14c2:7f:1b2a:9c11/64", "192.168.21.57/24"}},
		{Name: "utun6", Addrs: []string{"198.18.0.1/16"}},
	}}
	route := func() (net.IP, error) { return net.ParseIP("198.18.0.1"), nil }
	for _, local := range []net.IP{net.ParseIP("198.18.0.1"), net.ParseIP("127.0.0.1")} {
		got := Management(local, false, inv, route)
		if got == nil || got.IP != "192.168.21.57" || got.Interface != "en0" {
			t.Fatalf("management from %v = %+v", local, got)
		}
	}
	// With no NIC address to stand in, there is no usable address.
	if got := Management(net.ParseIP("198.18.0.1"), false, proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "utun6", Addrs: []string{"198.18.0.1/16"}}}}, route); got != nil {
		t.Fatalf("fake IP reported as management address: %+v", got)
	}
	// 198.20.0.1 is outside the /15 and stays as it is.
	if got := Management(net.ParseIP("198.20.0.1"), false, inv, route); got == nil || got.IP != "198.20.0.1" {
		t.Fatalf("an ordinary address was replaced: %+v", got)
	}
}

// A Mac Studio running OrbStack behind a proxy in TUN mode (seen in the field):
// the server connection and the default route leave through utun6 with a fake
// IP, OrbStack's bridge100 holds its VMs' gateway, and Tailscale is utun4.
// Picking bridge100 sent a peer model sync into the VM bridge ("host is down").
func TestManagementOnAMacBehindAProxyPicksTheNIC(t *testing.T) {
	ifaces := []Iface{
		{Name: "bridge100", MAC: "be:d0:74:1c:9a:64", Addrs: []string{"192.168.139.3/23"}},
		{Name: "en0", MAC: "5c:1b:f4:7a:10:22", Addrs: []string{"172.21.1.123/16"}},
		{Name: "utun4", Addrs: []string{"100.73.117.19/32"}},
		{Name: "utun6", Addrs: []string{"198.18.0.1/16"}},
		{Name: "vmenet0", MAC: "6e:12:5a:00:00:01"},
	}
	inv := Source{SysRoot: t.TempDir(), OS: "darwin", Interfaces: func() ([]Iface, error) { return ifaces, nil }}.Collect()
	var names []string
	for _, i := range inv.Interfaces {
		names = append(names, i.Name)
	}
	if strings.Join(names, ",") != "en0,utun4,utun6" {
		t.Fatalf("Mac inventory = %v, want the NIC and the tunnels (Tailscale is one), not VM plumbing", names)
	}
	route := func() (net.IP, error) { return net.ParseIP("198.18.0.1"), nil }
	if got := Management(net.ParseIP("198.18.0.1"), false, inv, route); got == nil || got.IP != "172.21.1.123" || got.Interface != "en0" {
		t.Fatalf("management = %+v, want en0's address", got)
	}
	// On Linux the same names are not filtered: a host bridge there may well
	// carry the node's address.
	linux := Source{SysRoot: t.TempDir(), OS: "linux", Interfaces: func() ([]Iface, error) { return ifaces[:1], nil }}.Collect()
	if len(linux.Interfaces) != 1 {
		t.Fatalf("linux inventory = %+v", linux.Interfaces)
	}
}
