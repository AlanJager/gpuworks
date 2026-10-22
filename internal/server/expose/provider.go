package expose

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose/cloudflared"
)

// Provider is one way to publish an entry point.
//
// The split matters: the supervisor owns everything that is the same whatever
// the provider — the single worker, the states, the backoff, the log rotation,
// the public probe, the shutdown. A provider owns only what differs: how the
// connector is launched, what credential it needs, how the public address is
// obtained, and what the operator should be told about it.
type Provider interface {
	ID() string
	// Validate rejects a configuration this provider cannot act on, before
	// anything is started or downloaded.
	Validate(cfg *Config) error
	// Launch resolves the connector: the binary to run (acquiring it if this
	// host has none), its arguments and its isolated environment. net carries
	// the automatic network preferences of this attempt — the operator is never
	// asked about IP version or protocol; the supervisor walks that ladder when
	// the edge connection fails.
	Launch(ctx context.Context, env *ProviderEnv, cfg *Config, net Network) (*Launch, error)
	// Ready is called after the connector has registered. It decides the
	// public address, or reports that the entry point cannot be published yet
	// (a named tunnel whose remote routes are not configured).
	Ready(ctx context.Context, env *ProviderEnv, cfg *Config, stream StreamState) (Ready, error)
}

// ProviderEnv is what a provider may use from the server.
type ProviderEnv struct {
	Store *Store
	// Origin is the address the connector must point at: the server's own
	// listening address, already normalised to something dialable.
	Origin string
	Deps   Deps
	Logf   func(format string, args ...any)
}

// Network is the automatic network preference for one attempt. Both fields are
// observations of what worked, never operator input.
type Network struct {
	// EdgeIPVersion is "4" when the edge connection should be forced to IPv4.
	EdgeIPVersion string
	// Protocol is "http2" when the QUIC path failed.
	Protocol string
}

// Launch is a resolved connector start.
type Launch struct {
	Binary  string
	Version string
	Args    []string
	// Env is the complete environment: the provider applies the isolation
	// policy here so the supervisor never has to know about it.
	Env []string
	// URL is the address the supervisor should probe once the connector
	// registers, when the provider already knows it. Empty means "take it from
	// the connector's output or from discovery".
	URL string
}

// Ready is the provider's answer to "is the entry point published, and where".
type Ready struct {
	URL string
	// State is StateReady or StateAwaitingRoute.
	State string
	// Message explains a non-ready answer, and carries the provider's note
	// about what the address can and cannot do.
	Message string
	// Candidates are discovered addresses when more than one could serve; the
	// operator picks one and only that choice is persisted.
	Candidates []string
	// Remote says this answer came from the connector's remote configuration.
	// Such an observation is only true for as long as the remote side says so:
	// a route can be published, repointed or removed while the entry point
	// runs, so the supervisor re-reads it instead of treating it as settled.
	Remote bool
}

// HealthChecker verifies that an address really serves this server. It is an
// interface so a test can decide what "reachable" means without a network:
// readiness is the one judgement this feature must never get wrong.
type HealthChecker interface {
	CheckPublic(ctx context.Context, publicURL string) error
	CheckLocal(ctx context.Context, origin string) error
}

// DepsFactory builds the supervisor's dependencies. It is a variable so an
// in-process harness can supply a fake connector and a fake health checker;
// production uses NewDeps.
var DepsFactory = NewDeps

// Deps are the injectable edges of the package.
type Deps struct {
	Now    func() time.Time
	HTTP   *http.Client
	Prober HealthChecker
	Helper HelperCommand
}

// NewDeps returns the production dependencies.
func NewDeps() Deps {
	return Deps{
		Now:    func() time.Time { return time.Now().UTC() },
		HTTP:   cloudflared.DefaultClient(),
		Prober: NewProber(),
		Helper: SelfHelper(),
	}
}

// providerFor returns the implementation of a provider ID.
func providerFor(id string) (Provider, error) {
	switch id {
	case ProviderQuick:
		return quickProvider{}, nil
	case ProviderNamed:
		return &namedProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", id)
	}
}
