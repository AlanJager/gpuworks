package selfinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// probeHealth GETs an unauthenticated readiness endpoint and returns the
// version the server reports there ("" when the body carries none).
func probeHealth(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var health struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &health)
	return strings.TrimSpace(health.Version), nil
}

// waitHealthy polls url until it answers or the health timeout is spent. The
// wait is counted in attempts so an injected zero-cost sleep keeps tests quick.
//
// A wait on a nohup installation ends as soon as the process svc.Start left
// behind is gone: nothing supervises that mode, so no later attempt can
// answer and the rest of the budget would be spent on a server that no longer
// exists — a release that starts and exits (a missing shared library, a
// config it rejects) used to keep an operator waiting out the whole minute
// before the rollback. A systemd unit is Restart=always, so there the budget
// is exactly what gives a crashing service its retries, and the wait stands.
//
// want, when set, is the version the started binary must report. A server
// that is still answering on the port - one the stop could not reach, or
// that outlived it - is a healthy endpoint of the wrong version, and taking
// its answer reported an upgrade that never ran.
func (i *Installer) waitHealthy(ctx context.Context, svc Service, url, want string) error {
	const interval = time.Second
	attempts := int(i.deps.healthTimeout() / interval)
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		got, err := i.deps.Health(ctx, url)
		if err == nil && want != "" && got != "" && got != want {
			err = fmt.Errorf("the server answering there reports version %s, not %s", got, want)
		}
		if err != nil {
			lastErr = err
			// A service manager starts a crashed server again; only the
			// detached process is gone for good once it exits.
			if svc.Mode == ModeNohup && !svc.Active(ctx) {
				return fmt.Errorf("%s did not become ready: the server is no longer running (check %s): %w", url, i.layout.LogFile(), err)
			}
			i.deps.sleep(ctx, interval)
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out after %s", i.deps.healthTimeout())
	}
	return fmt.Errorf("%s did not become ready: %w", url, lastErr)
}

// splitAddr normalises a listen address. ":8800" and "8800" both mean "all
// interfaces on 8800"; anything else must be a host:port pair.
func splitAddr(addr string) (host, port string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", fmt.Errorf("invalid --addr: empty")
	}
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		if p := strings.TrimPrefix(addr, ":"); p != "" && !strings.ContainsAny(addr, ":/[]") {
			return "", p, nil
		}
		return "", "", fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if port == "" {
		return "", "", fmt.Errorf("invalid --addr %q: no port", addr)
	}
	return host, port, nil
}

// healthURL maps a listen address to a loopback URL that reaches it. A wildcard
// or empty host means "all interfaces", which loopback reaches; an explicit
// host is used as given, because the server may be bound to one address only.
func healthURL(addr string) (string, error) {
	host, port, err := splitAddr(addr)
	if err != nil {
		return "", err
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
