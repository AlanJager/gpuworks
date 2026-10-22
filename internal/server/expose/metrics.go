package expose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// metricsConfig is the subset of cloudflared's local configuration endpoint
// this feature reads. The endpoint is the connector's own view of the remote
// configuration it received; nothing else on the host may be queried, which is
// why the address is chosen by us and belongs to the child we started.
//
// The version field starts at -1 and only moves once remote configuration has
// actually arrived — a connector that is online but has no routes yet must not
// be mistaken for a published entry point.
type metricsConfig struct {
	Version int `json:"version"`
	Config  struct {
		Ingress []metricsIngress `json:"ingress"`
	} `json:"config"`
}

type metricsIngress struct {
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
	Path     string `json:"path"`
}

// fetchMetricsConfig reads the connector's local configuration.
func fetchMetricsConfig(ctx context.Context, client *http.Client, addr string) (*metricsConfig, error) {
	if client == nil {
		client = &http.Client{}
	}
	target := "http://" + addr + "/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the connector's local configuration endpoint did not answer: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the connector's local configuration endpoint answered HTTP %d", resp.StatusCode)
	}
	var cfg metricsConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		// A version of cloudflared whose endpoint returns something else is not
		// usable for discovery; saying so is better than guessing a hostname.
		return nil, fmt.Errorf("the connector's local configuration is not readable (%w); check the connector log, and use cloudflare-quick for a temporary address", err)
	}
	return &cfg, nil
}

// selectHostnames returns the hostnames of the remote routes that publish this
// server, in a stable order.
//
// A route counts only when it names a concrete hostname — a catch-all rule has
// none, and a wildcard does not name one site — and when its service points at
// our origin. Anything else belongs to another service on the same tunnel, and
// publishing it here would hand the operator someone else's address.
func selectHostnames(cfg *metricsConfig, origin string) []string {
	if cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, rule := range cfg.Config.Ingress {
		host := strings.TrimSpace(rule.Hostname)
		if host == "" || strings.Contains(host, "*") {
			continue
		}
		// A path-only rule does not publish a whole site.
		if strings.TrimSpace(rule.Path) != "" {
			continue
		}
		if !sameOrigin(rule.Service, origin) {
			continue
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

// sameOrigin reports whether a route's service points at our origin.
//
// The comparison is by host and port with the loopback spellings treated as
// equivalent, because the operator writes the service by hand in the
// Cloudflare dashboard and `localhost:8800`, `127.0.0.1:8800` and `[::1]:8800`
// all mean this server.
func sameOrigin(service, origin string) bool {
	svcHost, svcPort, ok := splitService(service)
	if !ok {
		return false
	}
	ourURL, err := url.Parse(origin)
	if err != nil {
		return false
	}
	ourPort := ourURL.Port()
	if svcPort != ourPort {
		return false
	}
	return loopbackEquivalent(svcHost, ourURL.Hostname())
}

// splitService accepts both the URL form ("http://127.0.0.1:8800") and the bare
// host:port cloudflared also allows.
func splitService(service string) (host, port string, ok bool) {
	service = strings.TrimSpace(service)
	if service == "" {
		return "", "", false
	}
	if strings.Contains(service, "://") {
		parsed, err := url.Parse(service)
		if err != nil {
			return "", "", false
		}
		service = parsed.Host
	}
	host, port, err := net.SplitHostPort(service)
	if err != nil {
		return "", "", false
	}
	return strings.Trim(host, "[]"), port, true
}

// loopbackEquivalent reports whether two host spellings name the same local
// interface.
func loopbackEquivalent(a, b string) bool {
	return normalizeHost(a) == normalizeHost(b)
}

func normalizeHost(host string) string {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		return "loopback"
	}
	return strings.ToLower(host)
}

// freeLoopbackPort reserves a loopback port for the connector's metrics
// endpoint. The port has to belong to the child we start — the plan forbids
// scanning for someone else's cloudflared — so we pick it and pass it in.
func freeLoopbackPort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "", err
	}
	return port, nil
}
