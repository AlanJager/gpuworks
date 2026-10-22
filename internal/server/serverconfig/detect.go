package serverconfig

import (
	"errors"
	"net"
	"strings"
)

// Host is what address detection needs from the machine; tests replace it.
type Host struct {
	// RouteIP returns the local address the default route leaves from, or an
	// error when there is no default route.
	RouteIP func() (net.IP, error)
	// Interfaces lists the machine's interfaces and their addresses.
	Interfaces func() ([]Interface, error)
}

// Interface is one network interface and its addresses.
type Interface struct {
	Name  string
	Up    bool
	Addrs []net.IP
}

// SystemHost reads the running machine.
func SystemHost() Host {
	return Host{RouteIP: routeIP, Interfaces: systemInterfaces}
}

// ErrNoAddress means detection found nothing to bind; the operator must name
// an address at install time.
var ErrNoAddress = errors.New("no usable local address: there is no default route and no non-loopback interface; pass --addr to gpuworks install")

// DetectIP picks the address a fresh install binds: the one the default route
// leaves from, else the first address of the first physical, up interface.
// Air-gapped networks often have no default route, hence the fallback.
func DetectIP(h Host) (net.IP, error) {
	if h.RouteIP != nil {
		if ip, err := h.RouteIP(); err == nil && ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip, nil
		}
	}
	if h.Interfaces == nil {
		return nil, ErrNoAddress
	}
	ifaces, err := h.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if !iface.Up || virtualInterface(iface.Name) {
			continue
		}
		for _, ip := range iface.Addrs {
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return ip, nil
			}
		}
	}
	return nil, ErrNoAddress
}

// LocalIPs lists every address on the machine, for checking that a listen
// address can be bound before it is saved.
func LocalIPs(h Host) (map[string]bool, error) {
	out := map[string]bool{}
	if h.Interfaces == nil {
		return out, nil
	}
	ifaces, err := h.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		for _, ip := range iface.Addrs {
			out[ip.String()] = true
		}
	}
	return out, nil
}

var virtualPrefixes = []string{"lo", "docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "tun", "tap", "kube"}

func virtualInterface(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// routeIP asks the kernel which local address a packet to a public address
// would leave from. Dialing UDP sends nothing.
func routeIP() (net.IP, error) {
	conn, err := net.Dial("udp", "223.5.5.5:53")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.New("no local UDP address")
	}
	return addr.IP, nil
}

func systemInterfaces() ([]Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		item := Interface{Name: iface.Name, Up: iface.Flags&net.FlagUp != 0}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				item.Addrs = append(item.Addrs, n.IP)
			}
		}
		out = append(out, item)
	}
	return out, nil
}
