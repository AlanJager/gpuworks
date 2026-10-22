// Package nodeaddr validates routable hosts and tunneled agent endpoints.
package nodeaddr

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// EndpointInfo is a host plus an optional externally reachable TCP port.
// Scheme is intentionally discarded: server_dial opens a raw TCP connection,
// including when the user entered an http(s) URL for an ngrok TCP tunnel.
type EndpointInfo struct {
	Host    string
	Port    int
	HasPort bool
}

// ParseEndpoint accepts a host, host:port, or an http(s) URL containing a
// host[:port]. Paths, query strings, credentials, and fragments are rejected.
func ParseEndpoint(raw string) (EndpointInfo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return EndpointInfo{}, fmt.Errorf("host is required")
	}
	if strings.ContainsAny(raw, "\\") || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return EndpointInfo{}, fmt.Errorf("endpoint must not contain whitespace or backslashes")
	}
	hostPort := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return EndpointInfo{}, fmt.Errorf("invalid endpoint URL: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return EndpointInfo{}, fmt.Errorf("endpoint URL scheme must be http or https")
		}
		if u.User != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return EndpointInfo{}, fmt.Errorf("endpoint URL must contain only a host and optional port")
		}
		hostPort = u.Host
	}

	host := hostPort
	port := 0
	hasPort := false
	if h, p, err := net.SplitHostPort(hostPort); err == nil {
		host = h
		parsed, convErr := strconv.Atoi(p)
		if convErr != nil || parsed < 1 || parsed > 65535 {
			return EndpointInfo{}, fmt.Errorf("invalid endpoint port %q", p)
		}
		port, hasPort = parsed, true
	} else if strings.HasPrefix(hostPort, "[") || strings.HasSuffix(hostPort, "]") {
		if strings.HasPrefix(hostPort, "[") && strings.HasSuffix(hostPort, "]") {
			host = hostPort[1 : len(hostPort)-1]
		} else {
			return EndpointInfo{}, fmt.Errorf("invalid bracketed host")
		}
	} else if strings.Count(hostPort, ":") == 1 {
		parts := strings.SplitN(hostPort, ":", 2)
		if parsed, convErr := strconv.Atoi(parts[1]); convErr == nil {
			if parsed < 1 || parsed > 65535 {
				return EndpointInfo{}, fmt.Errorf("invalid endpoint port %q", parts[1])
			}
			host, port, hasPort = parts[0], parsed, true
		} else {
			return EndpointInfo{}, fmt.Errorf("invalid endpoint port %q", parts[1])
		}
	}
	normalized, err := Host(host)
	if err != nil {
		return EndpointInfo{}, err
	}
	return EndpointInfo{Host: normalized, Port: port, HasPort: hasPort}, nil
}

// Endpoint normalizes an endpoint for storage or a shell command. It removes
// an optional URL scheme while preserving an explicitly supplied port.
func Endpoint(raw string) (string, error) {
	info, err := ParseEndpoint(raw)
	if err != nil {
		return "", err
	}
	if !info.HasPort {
		return info.Host, nil
	}
	return net.JoinHostPort(info.Host, strconv.Itoa(info.Port)), nil
}

// Host normalizes a hostname, IPv4 address, or raw/bracketed IPv6 address.
// Ports belong to the separate server-dial and distributed master settings.
func Host(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("host is required")
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("host must not contain whitespace")
	}
	if strings.ContainsAny(raw, "/\\") || strings.Contains(raw, "://") {
		return "", fmt.Errorf("host must not be a URL or path")
	}

	host := raw
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if len(host) < 3 || !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return "", fmt.Errorf("invalid bracketed IPv6 address")
		}
		host = host[1 : len(host)-1]
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return "", fmt.Errorf("brackets are only valid around IPv6 addresses")
		}
		return host, nil
	}

	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	if strings.Contains(host, ":") {
		if _, _, err := net.SplitHostPort(host); err == nil {
			return "", fmt.Errorf("host must not include a port")
		}
		return "", fmt.Errorf("invalid host or IP address")
	}
	if !validHostname(host) {
		return "", fmt.Errorf("invalid hostname")
	}
	return host, nil
}

// RoutableHost additionally rejects addresses that cannot identify a peer in
// a multi-node deployment. Private network addresses remain valid.
func RoutableHost(raw string) (string, error) {
	info, err := ParseEndpoint(raw)
	if err != nil {
		return "", err
	}
	if info.HasPort {
		return "", fmt.Errorf("host must not include a port")
	}
	host := info.Host
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || lower == "localhost.localdomain" || strings.HasSuffix(lower, ".localhost") {
		return "", fmt.Errorf("loopback addresses are not routable between nodes")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return "", fmt.Errorf("loopback or unspecified addresses are not routable between nodes")
	}
	return host, nil
}

// RoutableEndpointHost validates an endpoint and returns its host, allowing an
// explicit port for tunneled agent connections.
func RoutableEndpointHost(raw string) (string, error) {
	info, err := ParseEndpoint(raw)
	if err != nil {
		return "", err
	}
	host := info.Host
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || lower == "localhost.localdomain" || strings.HasSuffix(lower, ".localhost") {
		return "", fmt.Errorf("loopback addresses are not routable between nodes")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return "", fmt.Errorf("loopback or unspecified addresses are not routable between nodes")
	}
	return host, nil
}

func validHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	trimmed := strings.TrimSuffix(host, ".")
	if trimmed == "" {
		return false
	}
	for _, label := range strings.Split(trimmed, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// LocalBaseURL guesses the base URL other hosts on the network can use to reach
// a server listening on addr ("http://10.0.0.5:8800"). It detects the address
// of the default route by asking the kernel to pick a source address for an
// outbound UDP dial; no packet is sent.
//
// This is a hint, not configuration: `gpuworks install` prints it for a
// wildcard listen address so the operator knows where to open the console. A
// host with no default route (an isolated cluster, a
// machine whose only interface is loopback) has no such URL and gets "".
func LocalBaseURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	conn, err := net.Dial("udp", "223.5.5.5:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	localIP, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localIP.IP.IsUnspecified() {
		return ""
	}
	return "http://" + net.JoinHostPort(localIP.IP.String(), port)
}
