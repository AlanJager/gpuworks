package serverconfig

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// The functions a listener can serve. A request is answered only on a
// listener that carries its function, so the console can live on the
// management network while the inference API lives on a business network.
const (
	FunctionConsole = "console" // Web UI and the management API
	FunctionAgent   = "agent"   // node connections, install script, agent downloads
	FunctionGateway = "gateway" // the OpenAI-compatible /v1 API
)

// Functions lists every function in a stable order.
var Functions = []string{FunctionConsole, FunctionAgent, FunctionGateway}

// Listen is the listen section: IP:port addresses per function. Agent and
// gateway inherit console when left out, which is the common single-address
// install.
type Listen struct {
	Console []string `yaml:"console,omitempty" json:"console"`
	Agent   []string `yaml:"agent,omitempty" json:"agent"`
	Gateway []string `yaml:"gateway,omitempty" json:"gateway"`
}

// Empty reports whether no function has an address.
func (l Listen) Empty() bool {
	return len(l.Console) == 0 && len(l.Agent) == 0 && len(l.Gateway) == 0
}

// Effective fills agent and gateway from console when they are left out.
func (l Listen) Effective() Listen {
	out := Listen{Console: clean(l.Console), Agent: clean(l.Agent), Gateway: clean(l.Gateway)}
	if len(out.Agent) == 0 {
		out.Agent = append([]string(nil), out.Console...)
	}
	if len(out.Gateway) == 0 {
		out.Gateway = append([]string(nil), out.Console...)
	}
	return out
}

// Of returns the effective addresses of one function.
func (l Listen) Of(function string) []string {
	e := l.Effective()
	switch function {
	case FunctionConsole:
		return e.Console
	case FunctionAgent:
		return e.Agent
	case FunctionGateway:
		return e.Gateway
	}
	return nil
}

func clean(addrs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// Validate checks every address is an IP literal with a port, that console has
// one when anything does, and that a wildcard does not share its port with a
// specific address (the kernel refuses to bind both).
func (l Listen) Validate() error {
	if l.Empty() {
		return nil
	}
	if len(clean(l.Console)) == 0 {
		return errors.New("listen.console needs at least one address")
	}
	wildcard := map[string]string{}
	specific := map[string]string{}
	for _, fn := range Functions {
		for _, addr := range l.Of(fn) {
			ip, port, err := SplitAddr(addr)
			if err != nil {
				return fmt.Errorf("listen.%s: %w", fn, err)
			}
			if ip.IsUnspecified() {
				wildcard[port] = addr
			} else {
				specific[port] = addr
			}
		}
	}
	for port, w := range wildcard {
		if s, ok := specific[port]; ok && !isLoopback(s) {
			return fmt.Errorf("listen: %s and %s share port %s; a wildcard address already covers every address on its port", w, s, port)
		}
	}
	return nil
}

// SplitAddr parses one IP:port listen address.
func SplitAddr(addr string) (net.IP, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return nil, "", fmt.Errorf("%q is not IP:port", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, "", fmt.Errorf("%q: %q is not an IP address", addr, host)
	}
	if n, err := net.LookupPort("tcp", port); err != nil || n <= 0 {
		return nil, "", fmt.Errorf("%q: invalid port", addr)
	}
	return ip, port, nil
}

func isLoopback(addr string) bool {
	ip, _, err := SplitAddr(addr)
	return err == nil && ServesEverything(ip)
}

// ServesEverything reports whether connections to ip get every function:
// 127.0.0.1 and ::1, which only the machine itself can reach. Other loopback
// addresses are ordinary listen addresses.
func ServesEverything(ip net.IP) bool {
	return ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback)
}

// Binding is one socket to open and the functions it serves.
type Binding struct {
	Addr      string
	Functions []string
}

// Serves reports whether the binding answers a function.
func (b Binding) Serves(function string) bool {
	for _, f := range b.Functions {
		if f == function {
			return true
		}
	}
	return false
}

// Bindings merges the listen section into sockets: one per distinct address,
// serving every function that lists it. 127.0.0.1 is always bound on every
// port in use and serves everything, because install, upgrade, health checks
// and an agent on the same machine reach the server there; a wildcard on a
// port already covers loopback.
func Bindings(l Listen) []Binding {
	e := l.Effective()
	byAddr := map[string][]string{}
	var order []string
	add := func(addr, fn string) {
		if _, ok := byAddr[addr]; !ok {
			order = append(order, addr)
		}
		for _, f := range byAddr[addr] {
			if f == fn {
				return
			}
		}
		byAddr[addr] = append(byAddr[addr], fn)
	}
	for _, fn := range Functions {
		for _, addr := range e.Of(fn) {
			add(canonical(addr), fn)
		}
	}
	ports := map[string]bool{}
	covered := map[string]bool{}
	wildcard := map[string]bool{}
	for _, addr := range order {
		ip, port, _ := SplitAddr(addr)
		ports[port] = true
		if ip.IsUnspecified() {
			wildcard[port] = true
		}
		if ip.IsUnspecified() || ip.Equal(net.IPv4(127, 0, 0, 1)) {
			covered[port] = true
		}
	}
	// A wildcard already accepts 127.0.0.1 on its port; binding both fails.
	kept := order[:0]
	for _, addr := range order {
		ip, port, _ := SplitAddr(addr)
		if wildcard[port] && !ip.IsUnspecified() && ServesEverything(ip) {
			continue
		}
		kept = append(kept, addr)
	}
	order = kept
	var loopbackPorts []string
	for port := range ports {
		if !covered[port] {
			loopbackPorts = append(loopbackPorts, port)
		}
	}
	sort.Strings(loopbackPorts)
	for _, port := range loopbackPorts {
		order = append(order, net.JoinHostPort("127.0.0.1", port))
	}
	out := make([]Binding, 0, len(order))
	for _, addr := range order {
		fns := byAddr[addr]
		if isLoopback(addr) {
			fns = append([]string(nil), Functions...)
		}
		out = append(out, Binding{Addr: addr, Functions: fns})
	}
	return out
}

func canonical(addr string) string {
	ip, port, err := SplitAddr(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}
	return net.JoinHostPort(ip.String(), port)
}

// FunctionOf classifies a request path. The empty string means every listener
// answers it (health checks).
func FunctionOf(path string) string {
	switch {
	case path == "/healthz":
		return ""
	case path == "/api/v1/agent/connect", path == "/install.sh", strings.HasPrefix(path, "/dl/"):
		return FunctionAgent
	case path == "/v1", strings.HasPrefix(path, "/v1/"):
		return FunctionGateway
	}
	return FunctionConsole
}
