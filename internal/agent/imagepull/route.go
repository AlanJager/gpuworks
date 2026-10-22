package imagepull

import (
	"context"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// routeFor is the network path a probe takes: the one the pull will take.
// Docker pulls in its daemon, so that is the daemon's proxy as it reports it
// (a daemon with no proxy goes direct, whatever the agent's own environment
// says). Every other runtime pulls in a process the agent starts, which uses
// the node's configured proxy, and without one the agent's environment.
func (p *Puller) routeFor(ctx context.Context, driver runtime.Driver) registryprobe.Route {
	net := p.network()
	if isDocker(driver) && p.cfg.DaemonProxy != nil {
		if httpProxy, httpsProxy, noProxy, ok := p.cfg.DaemonProxy(ctx); ok {
			if httpProxy == "" && httpsProxy == "" {
				return registryprobe.DirectRoute
			}
			return registryprobe.RouteFor(unmasked(httpProxy, net), unmasked(httpsProxy, net), noProxy)
		}
	}
	return registryprobe.RouteFor(net.HTTPProxy, net.HTTPSProxy, net.NoProxy)
}

// unmasked restores the credentials a daemon hides: docker info prints a
// proxy's user and password as "xxxxx". When the node's own configuration
// names the same proxy, its credentials are the ones the daemon uses.
func unmasked(observed string, net proto.NetworkConfig) string {
	obs, ok := parseProxy(observed)
	if !ok || obs.User == nil {
		return observed
	}
	if pw, _ := obs.User.Password(); pw != "xxxxx" && obs.User.Username() != "xxxxx" {
		return observed
	}
	for _, configured := range []string{net.HTTPSProxy, net.HTTPProxy} {
		if conf, ok := parseProxy(configured); ok && conf.User != nil && conf.Host == obs.Host && conf.Scheme == obs.Scheme {
			return configured
		}
	}
	// No credentials to restore: send none rather than the mask.
	obs.User = nil
	return obs.String()
}

func parseProxy(raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, false
	}
	return u, true
}
