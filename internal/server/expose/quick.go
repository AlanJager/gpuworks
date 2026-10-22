package expose

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/expose/cloudflared"
)

// quickProvider publishes a temporary address through Cloudflare's quick
// tunnel: no account, no domain, and an address that changes every time the
// connector registers.
type quickProvider struct{}

func (quickProvider) ID() string { return ProviderQuick }

func (quickProvider) Validate(cfg *Config) error {
	if cfg.TokenFile != "" {
		return fmt.Errorf("%s takes no token; remove token_file from the configuration", ProviderQuick)
	}
	if cfg.Hostname != "" {
		return fmt.Errorf("%s cannot pin a hostname; the address is assigned by Cloudflare", ProviderQuick)
	}
	return nil
}

func (quickProvider) Launch(ctx context.Context, env *ProviderEnv, cfg *Config, net Network) (*Launch, error) {
	binary, version, err := cloudflared.Acquire(ctx, env.Deps.HTTP, cloudflared.Request{
		BinDir: env.Store.BinDir(),
		Logf:   env.Logf,
	})
	if err != nil {
		return nil, err
	}
	home, err := env.Store.prepareIsolation()
	if err != nil {
		return nil, err
	}
	// The argument order is the one the verified cloudflared version accepts;
	// --no-autoupdate keeps a supervised connector from replacing its own
	// binary underneath the version we checked.
	args := []string{"tunnel", "--url", env.Origin, "--no-autoupdate"}
	if net.EdgeIPVersion != "" {
		args = append(args, "--edge-ip-version", net.EdgeIPVersion)
	}
	if net.Protocol != "" {
		args = append(args, "--protocol", net.Protocol)
	}
	return &Launch{
		Binary:  binary,
		Version: version,
		Args:    args,
		Env:     connectorEnv(home),
	}, nil
}

// Ready for a quick tunnel: the address is whatever the connector printed, and
// it is usable only once that connector has registered — the URL appears
// before registration, and a link handed out in that window answers 530.
func (quickProvider) Ready(ctx context.Context, env *ProviderEnv, cfg *Config, stream StreamState) (Ready, error) {
	if stream.Registration == nil {
		return Ready{}, errors.New("the connector has not registered with the Cloudflare edge yet")
	}
	if stream.URL == "" {
		return Ready{}, errors.New("the connector registered but printed no quick tunnel address")
	}
	return Ready{URL: stream.URL, State: StateReady, Message: quickLimits}, nil
}

// quickLimits is what every operator of a quick entry point has to know, and
// what the CLI prints on every ready — including the ordinary `status`.
const quickLimits = "temporary address: it changes when the connector restarts, streaming (SSE) is not supported, and Cloudflare gives no availability guarantee — for nodes use the server's own stable address"
