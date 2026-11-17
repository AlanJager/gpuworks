// Package pluginbridge joins the parts of the plugin host into one service:
// the installed packages (extpkg), the core's pushed state (pushstate) and
// the plugin processes (pluginhost). It decides nothing those packages
// decide; it hands each what the others know.
//
// At boot it verifies every installed package, loads pushed state with
// them before anything is served, and hands the server the evaluators to
// register (featuregate.Source, the node insert guard, the scheduling
// holder, the console banners). Run then starts the enabled resident
// plugins, in parallel and without holding up the server, and tells pushed
// state which owners are up. A push arriving on a plugin's host connection
// is applied as the package bound to that connection, with the grants of
// the certificate it verified under and the permissions its administrator
// approved. See docs/dev/extensions.md and ADR 0071.
package pluginbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// FeatureTopic is the bus topic announcing that feature availability or
// banners may have changed, for the console to read its manifest again.
const FeatureTopic = "feature"

// devCoreVersion names a development build in the handshake, whose own
// version ("dev") is not a release version.
const devCoreVersion = "v0.0.0-dev"

// coreVersionPattern is the handshake's grammar for the server version
// (pkg/extapi, HandshakeRequest).
var coreVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.+-]{1,64})?$`)

// Config is what Open needs.
type Config struct {
	// Packages are the installed packages in the data directory.
	Packages *extpkg.Host
	// State builds pushed state; Open fills in Packages.
	State pushstate.Config
	// Host builds the plugin host; Open fills in Core, State and OnState.
	Host pluginhost.Config
	// Core describes this server in the handshake; Open fills in Offer
	// from the capabilities the server offers (extpkg.Offer).
	Core extapi.CoreInfo
	// RequireIsolation is the site's plugins.require_isolation.
	RequireIsolation string
	Log              *slog.Logger
}

// Bridge is the plugin host of one server.
type Bridge struct {
	cfg   Config
	log   *slog.Logger
	pkgs  *extpkg.Host
	state *pushstate.State
	host  *pluginhost.Host
	// features are the feature keys declared at boot.
	features []featuregate.Feature

	// admin serializes the administration calls (install, enable,
	// disable, uninstall) with each other and with Run's start.
	admin sync.Mutex

	mu sync.Mutex
	// entries are the installed packages as last verified, by id.
	entries map[string]extpkg.Entry
	// pushers are the identities of the running plugins, by package id,
	// bound when each is started.
	pushers map[string]identity
	// running is true while Run runs: plugins enabled meanwhile start at
	// once.
	running bool
}

// identity is who a plugin's pushes come from.
type identity struct {
	version string
	pusher  pushstate.Pusher
}

// Open verifies the installed packages (rehashing every file), removes
// what an interrupted install left, and loads pushed state, so what was
// pushed is enforced before the server serves a request and before any
// plugin starts. It starts no plugin.
func Open(ctx context.Context, cfg Config) (*Bridge, error) {
	if cfg.Packages == nil {
		return nil, errors.New("pluginbridge: Config.Packages is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	b := &Bridge{cfg: cfg, log: cfg.Log, pkgs: cfg.Packages, entries: map[string]extpkg.Entry{}, pushers: map[string]identity{}}
	if err := b.pkgs.Dir.Cleanup(); err != nil {
		return nil, fmt.Errorf("plugins: remove interrupted installs: %w", err)
	}
	entries, err := b.pkgs.List()
	if err != nil {
		return nil, fmt.Errorf("plugins: list installed packages: %w", err)
	}
	for _, e := range entries {
		b.entries[e.ID] = e
		if e.Err != nil {
			b.log.Warn("installed plugin does not verify; what it restricted stays enforced and it does not run", "plugin", e.ID, "err", e.Err)
		}
	}
	stateCfg := cfg.State
	stateCfg.Packages = b.statePackagesLocked()
	if stateCfg.Log == nil {
		stateCfg.Log = cfg.Log.With("component", "pushed_state")
	}
	b.state = pushstate.New(stateCfg)
	if err := b.state.Load(ctx); err != nil {
		return nil, err
	}
	b.features = b.state.Features()
	hostCfg := cfg.Host
	core := cfg.Core
	core.Offer = extpkg.Offer()
	if !coreVersionPattern.MatchString(core.Version) {
		core.Version = devCoreVersion
	}
	hostCfg.Core = func() extapi.CoreInfo { c := core; c.Offer = maps.Clone(core.Offer); return c }
	hostCfg.State = stateHost{b}
	hostCfg.OnState = b.onState
	if hostCfg.Log == nil {
		hostCfg.Log = cfg.Log.With("component", "plugins")
	}
	if b.host, err = pluginhost.New(hostCfg); err != nil {
		return nil, err
	}
	return b, nil
}

// State is the pushed state the server's hooks answer from.
func (b *Bridge) State() *pushstate.State { return b.state }

// Features are the feature keys installed packages declare, for the
// feature gate. The gate is built once at boot, so keys of a package
// installed later are declared at the next start.
func (b *Bridge) Features() []featuregate.Feature { return slices.Clone(b.features) }

// Run runs pushed state's writer tick, timelines and capacity selection,
// starts every enabled resident plugin, and stops them all when ctx ends.
// Pushed state stops after the plugins, so the moment each went down is
// persisted and a restart of the server never renews its grace. It may
// run again after it returned.
func (b *Bridge) Run(ctx context.Context) error {
	stateCtx, stopState := context.WithCancel(context.WithoutCancel(ctx))
	defer stopState()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := b.state.Run(stateCtx); err != nil {
			b.log.Error("pushed state stopped", "err", err)
		}
	}()
	b.admin.Lock()
	b.mu.Lock()
	b.running = true
	ids := slices.Sorted(maps.Keys(b.entries))
	b.mu.Unlock()
	for _, id := range ids {
		b.startIfEnabled(id)
	}
	b.admin.Unlock()

	<-ctx.Done()
	b.admin.Lock()
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	b.stopAll()
	b.admin.Unlock()
	stopState()
	<-done
	return nil
}

// stopAll stops every supervised plugin, each in parallel, waiting for all.
func (b *Bridge) stopAll() {
	var wg sync.WaitGroup
	for _, st := range b.host.List() {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), stopWait)
			defer cancel()
			if err := b.host.Stop(ctx, id); err != nil && !errors.Is(err, pluginhost.ErrUnknownPlugin) {
				b.log.Warn("plugin did not stop in time", "plugin", id, "err", err)
			}
		}(st.ID)
	}
	wg.Wait()
}

// onState tells pushed state whether a plugin is up: running and degraded
// count as up, every other state as down (ADR 0071, decision 9).
func (b *Bridge) onState(st pluginhost.Status) {
	if st.State.Up() {
		b.state.OwnerUp(st.ID, st.Generation)
		return
	}
	b.state.OwnerDown(st.ID)
}
