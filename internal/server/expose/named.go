package expose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/expose/cloudflared"
)

// namedProvider publishes a fixed address through a Cloudflare tunnel the
// operator already created: the domain, the tunnel and its published route
// live in their Cloudflare account, and the only thing this side needs is the
// tunnel token.
//
// The token is a run credential, not an account API token: it can start this
// one tunnel and cannot create tunnels, domains or routes. The address is
// therefore not in the token — it is whatever route the operator published,
// which the connector receives and this code reads from the connector's own
// local configuration endpoint.
type namedProvider struct {
	// metricsAddr is the loopback address of the connector started for this
	// attempt. It is per-attempt state: the port belongs to the child we just
	// started, never to some other cloudflared on the host.
	metricsAddr string
}

func (*namedProvider) ID() string { return ProviderNamed }

func (*namedProvider) Validate(cfg *Config) error {
	if cfg.TokenFile == "" {
		return errors.New("cloudflare-named needs the tunnel token; run `gpuworks expose up --provider cloudflare-named` and paste it")
	}
	info, err := os.Stat(cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("the tunnel token file %s is not readable: %w", cfg.TokenFile, err)
	}
	if info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("the tunnel token file %s is empty", cfg.TokenFile)
	}
	return nil
}

func (p *namedProvider) Launch(ctx context.Context, env *ProviderEnv, cfg *Config, net Network) (*Launch, error) {
	// The address is discovered through the connector's local configuration
	// endpoint, which was verified against one version: a PATH binary older
	// than that is skipped in favour of the controlled download instead of
	// being trusted to expose the same structure.
	binary, version, err := cloudflared.Acquire(ctx, env.Deps.HTTP, cloudflared.Request{
		BinDir:    env.Store.BinDir(),
		Supported: cloudflared.SupportsConfigEndpoint,
		Logf:      env.Logf,
	})
	if err != nil {
		return nil, err
	}
	home, err := env.Store.prepareIsolation()
	if err != nil {
		return nil, err
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("cannot reserve a loopback port for the connector's configuration endpoint: %w", err)
	}
	p.metricsAddr = metricsAddrFor(port)
	// The token goes in a file that cloudflared reads: never in the command
	// line, where every process on the host could read it, and never in the
	// environment, which the child would inherit into its own children.
	args := []string{
		"tunnel",
		"--no-autoupdate",
		"--metrics", p.metricsAddr,
		"run",
		"--token-file", cfg.TokenFile,
	}
	if net.EdgeIPVersion != "" {
		args = append(args, "--edge-ip-version", net.EdgeIPVersion)
	}
	if net.Protocol != "" {
		args = append(args, "--protocol", net.Protocol)
	}
	return &Launch{Binary: binary, Version: version, Args: args, Env: connectorEnv(home)}, nil
}

// metricsAddr is where the connector's configuration endpoint is expected to
// answer. It is a variable because a harness supplies its own endpoint: the
// real one is a loopback port reserved for the connector this attempt starts,
// which only the attempt knows.
var metricsAddrFor = func(port string) string { return "127.0.0.1:" + port }

// Ready for a named tunnel: the connector is online, and the address is the
// hostname of the published route that points at this server.
//
// Every answer here is read from the connector's remote configuration, so it is
// marked Remote: the supervisor keeps re-reading it while the entry point runs,
// because the operator can publish, repoint or remove the route at any time.
func (p *namedProvider) Ready(ctx context.Context, env *ProviderEnv, cfg *Config, stream StreamState) (Ready, error) {
	ready, err := p.discoverRoute(ctx, env, cfg, stream)
	if err != nil {
		return Ready{}, err
	}
	ready.Remote = true
	return ready, nil
}

// discoverRoute is Ready's body: what the connector's remote configuration
// currently publishes for this server.
func (p *namedProvider) discoverRoute(ctx context.Context, env *ProviderEnv, cfg *Config, stream StreamState) (Ready, error) {
	if stream.Registration == nil {
		return Ready{}, errors.New("the connector has not registered with the Cloudflare edge yet")
	}
	remote, err := fetchMetricsConfig(ctx, env.Deps.HTTP, p.metricsAddr)
	if err != nil {
		return Ready{}, err
	}
	if remote.Version <= 0 {
		// The connector is online but Cloudflare has sent it no configuration
		// yet. Keeping it running is the point: the operator publishes the
		// route in the dashboard and the connector picks it up.
		return Ready{
			State:   StateAwaitingRoute,
			Message: awaitingRouteMessage(env.Origin),
		}, nil
	}
	hostnames := selectHostnames(remote, env.Origin)
	switch {
	case len(hostnames) == 0:
		return Ready{State: StateAwaitingRoute, Message: awaitingRouteMessage(env.Origin)}, nil
	case len(hostnames) == 1:
		host := hostnames[0]
		if cfg.Hostname != "" && !strings.EqualFold(cfg.Hostname, host) {
			// The operator pinned a hostname that the remote configuration no
			// longer publishes: revoke ready rather than silently serving a
			// different site.
			return Ready{}, fmt.Errorf("%w: the route for %s is gone from the tunnel; published routes now point at %s", errRouteGone, cfg.Hostname, host)
		}
		return Ready{URL: "https://" + host, State: StateReady, Message: namedNote}, nil
	default:
		if cfg.Hostname != "" {
			for _, host := range hostnames {
				if strings.EqualFold(cfg.Hostname, host) {
					return Ready{URL: "https://" + cfg.Hostname, State: StateReady, Message: namedNote}, nil
				}
			}
			return Ready{}, fmt.Errorf("%w: the route for %s is gone from the tunnel; published routes now point at %s", errRouteGone, cfg.Hostname, strings.Join(hostnames, ", "))
		}
		// Several routes publish this server. Picking one here would hand the
		// operator an address they did not choose — a different site, on
		// someone else's domain — so the choice goes back to them.
		return Ready{
			State:      StateAwaitingRoute,
			Candidates: hostnames,
			Message:    "several published routes point at this server; choose one with `gpuworks expose up` (it is remembered)",
		}, nil
	}
}

// errRouteGone marks the one remote answer that is not a transient failure: the
// route this entry point was published under no longer exists. The supervisor
// revokes the address for it instead of waiting for the endpoint to answer
// again, which is the plan's "revoke ready before switching sites".
var errRouteGone = errors.New("the published route is gone")

// namedNote is what an operator of a named entry point should know.
const namedNote = "fixed address: the hostname stays the same while the route does not change, and streaming (SSE) works"

func awaitingRouteMessage(origin string) string {
	return fmt.Sprintf("the connector is online, but no published route points at %s yet: in the Cloudflare dashboard, open this tunnel's Routes and publish an application with your hostname and the service %s", origin, origin)
}
