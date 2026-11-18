package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Config describes a plugin's server part.
type Config struct {
	// ID, Version and ManifestSHA256 name the package build; the server
	// refuses a binary whose answer does not match the installed package.
	ID             string
	Version        string
	ManifestSHA256 extapi.Digest
	// Requires maps each capability the plugin cannot run without to the
	// versions it speaks; a server offering none of them is refused with
	// PLUGIN_CAPABILITY_MISSING. Uses lists optional capabilities the same
	// way; each is used at the newest version both sides speak, if any.
	Requires map[string]extapi.Range
	Uses     map[string]extapi.Range
	// Services are the optional services the plugin serves, sorted.
	Services []string
	// Init inspects the accepted handshake before the plugin answers it.
	// Returning an *Error refuses initialize with it; any other error ends
	// the plugin, which the server treats as a crash.
	Init func(ctx context.Context, p *Plugin) error
	// Start runs once initialize was answered, with a context that ends at
	// shutdown. A non-nil error ends the plugin.
	Start func(ctx context.Context, p *Plugin) error
	// Health answers the server's probe; nil answers ok.
	Health func(ctx context.Context) extapi.Health
	// Handler serves every other call (routes under /r/, services under
	// /gwx/v1/svc/) once the plugin is initialized. Its requests carry the
	// *Plugin (PluginFrom) and the caller's deadline.
	Handler http.Handler
	// Logger receives the SDK's own records; nil logs JSON lines to stderr,
	// which the server forwards to its log.
	Logger *slog.Logger
}

// ErrServerGone is returned when the server's end of a connection closed or
// the parent process is gone: the plugin should exit.
var ErrServerGone = errors.New("pluginsdk: the server is gone")

// Main runs the plugin under the server and exits the process: 0 after a
// shutdown the server asked for (or SIGTERM), 1 otherwise.
func Main(cfg Config) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	err := Run(ctx, cfg)
	stop()
	if err != nil && !errors.Is(err, context.Canceled) {
		logger(cfg).Error("plugin exits", "err", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Run opens the descriptors the server handed the plugin and serves until
// shutdown, the server's end closes, the parent is gone or ctx ends.
func Run(ctx context.Context, cfg Config) error {
	serve, host, err := connsFromEnv()
	if err != nil {
		return err
	}
	return serveConns(ctx, cfg, serve, host, time.Second)
}

// Serve runs the plugin on two connected sockets: serve, on which it
// answers the server, and host, on which it calls the host API. It is what
// Run does after opening the descriptors, without watching the parent;
// plugintest uses it in process.
func Serve(ctx context.Context, cfg Config, serve, host net.Conn) error {
	return serveConns(ctx, cfg, serve, host, 0)
}

func logger(cfg Config) *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

func (cfg Config) validate() error {
	r := extapi.HandshakeResponse{
		Protocol: extapi.ProtocolMajor, ID: cfg.ID, Version: cfg.Version,
		ManifestSHA256: cfg.ManifestSHA256, Services: cfg.Services,
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("pluginsdk: config: %w", err)
	}
	for name, rng := range cfg.Requires {
		if _, both := cfg.Uses[name]; both {
			return fmt.Errorf("pluginsdk: config: %s is both required and optional", name)
		}
		if rng.Min < 1 || rng.Max < rng.Min {
			return fmt.Errorf("pluginsdk: config: requires %s has range %d..%d", name, rng.Min, rng.Max)
		}
	}
	for name, rng := range cfg.Uses {
		if rng.Min < 1 || rng.Max < rng.Min {
			return fmt.Errorf("pluginsdk: config: uses %s has range %d..%d", name, rng.Min, rng.Max)
		}
	}
	return nil
}

// Plugin is the running plugin: the accepted handshake, the negotiated
// capabilities and the host API client.
type Plugin struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client

	// ready is set once initialize was accepted; req and use are written
	// before it and never after.
	ready atomic.Bool
	req   *extapi.HandshakeRequest
	use   map[string]int

	pushMu sync.Mutex
	held   map[string]extapi.HeldState
	own    map[string]uint64
}

// Request is the server's accepted initialize request.
func (p *Plugin) Request() *extapi.HandshakeRequest { return p.req }

// Use returns the negotiated version of capability, if it is used.
func (p *Plugin) Use(capability string) (int, bool) {
	v, ok := p.use[capability]
	return v, ok
}

// Has reports whether capability is used at version v or newer.
func (p *Plugin) Has(capability string, v int) bool {
	got, ok := p.use[capability]
	return ok && got >= v
}

// Logger is the plugin's logger (JSON lines on stderr by default).
func (p *Plugin) Logger() *slog.Logger { return p.log }

// HostClient is the HTTP/2 client of the host API connection, for host
// endpoints this package has no helper for. Request URLs need only a path.
func (p *Plugin) HostClient() *http.Client { return p.client }

type pluginKey struct{}

// PluginFrom returns the plugin serving a request passed to Config.Handler.
func PluginFrom(ctx context.Context) *Plugin {
	p, _ := ctx.Value(pluginKey{}).(*Plugin)
	return p
}

// negotiate picks, per capability, the newest version both the offer and
// the plugin speak. A required capability with no such version refuses.
func negotiate(offer map[string]extapi.Range, requires, uses map[string]extapi.Range) (map[string]int, *extapi.Error) {
	use := map[string]int{}
	pick := func(name string, want extapi.Range) (int, bool) {
		o, ok := offer[name]
		v := min(o.Max, want.Max)
		return v, ok && v >= max(o.Min, want.Min)
	}
	for _, name := range sortedNames(requires) {
		v, ok := pick(name, requires[name])
		if !ok {
			return nil, extapi.CapabilityMissing(name, requires[name].Min, requires[name].Max)
		}
		use[name] = v
	}
	for _, name := range sortedNames(uses) {
		if v, ok := pick(name, uses[name]); ok {
			use[name] = v
		}
	}
	return use, nil
}

func sortedNames(m map[string]extapi.Range) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
