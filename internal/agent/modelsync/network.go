package modelsync

import (
	"fmt"
	"net"
	"slices"
	"strings"
)

// LocalIP parses an address the server chose for a copy and checks it is one
// of this host's, so a receiver binds exactly the network the server picked
// and a sender dials from it. An empty address means no preference: the OS
// routing table decides.
func LocalIP(raw string) (net.IP, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return nil, fmt.Errorf("sync address %q is not an IP address", raw)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if local := ipFromAddr(addr); local != nil && local.Equal(ip) {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("sync address %s is not an address of this node", raw)
}

// ListenAddrs is where a receiver listens and what it advertises: only the
// chosen address, or every candidate when the server named none.
func ListenAddrs(bindIP string) (string, []string, error) {
	ip, err := LocalIP(bindIP)
	if err != nil {
		return "", nil, err
	}
	if ip != nil {
		return ip.String(), []string{ip.String()}, nil
	}
	return "0.0.0.0", CandidateHosts(), nil
}

// CandidateHosts lists global-unicast interface addresses, IPv4 first.
func CandidateHosts() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ip := ipFromAddr(addr); usableIP(ip) {
				ips = append(ips, ip)
			}
		}
	}
	sortIPs(ips)
	hosts := make([]string, 0, len(ips))
	for _, ip := range ips {
		hosts = append(hosts, ip.String())
	}
	return uniqueHosts(hosts)
}

func sortIPs(ips []net.IP) {
	slices.SortStableFunc(ips, func(a, b net.IP) int {
		if (a.To4() != nil) == (b.To4() != nil) {
			return strings.Compare(a.String(), b.String())
		}
		if a.To4() != nil {
			return -1
		}
		return 1
	})
}

func ipFromAddr(addr net.Addr) net.IP {
	value := addr.String()
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		value = value[:slash]
	}
	if zone := strings.LastIndex(value, "%"); zone >= 0 {
		value = value[:zone]
	}
	return net.ParseIP(value)
}

func usableIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}
