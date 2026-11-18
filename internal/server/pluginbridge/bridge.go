// Package pluginbridge joins the parts of the plugin host into one service:
// the installed packages (extpkg), the core's pushed state (pushstate) and
// the plugin processes (pluginhost). It decides nothing those packages
// decide; it hands each what the others know.
//
// At boot it verifies every installed package, loads pushed state with
// them before anything is served, declares their feature keys in the
// server's dynamic feature set (again after every install, enable,
// disable and uninstall), and hands the server the evaluators to register
// (the node insert guard, the scheduling holder, the console banners). Run
// then starts the enabled resident plugins, in parallel and without holding
// up the server, supervises the enabled on-demand ones until a call starts
// them (Call), and tells pushed state which resident owners are up. Routes
// is the table of plugin routes, compiled again on every change. A push arriving on a plugin's host connection
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
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/hostsetup"
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
	// Host builds the plugin host; Open fills in Core, State, OnState, the
	// clock, ready and own-state services and, unless set, the handshake's
	// Clock. Keys (derived keys) is the caller's.
	Host pluginhost.Config
	// Core describes this server in the handshake; Open fills in Offer
	// from the capabilities the server offers (extpkg.Offer).
	Core extapi.CoreInfo
	// RequireIsolation is the site's plugins.require_isolation.
	RequireIsolation string
	// HostSetup are the applied host setup records a package with
	// host_setup steps must match before its server part starts; the zero
	// value is hostsetup.DefaultRecords.
	HostSetup hostsetup.Records
	// Features is the server's dynamic feature set: the keys installed
	// packages declare, answered by pushed state. Nil declares none.
	Features *featuregate.DynamicSet
	// StaticFeatures name the module compiled into the server that
	// declares each static feature key; no package may declare one.
	StaticFeatures map[featuregate.Key]string
	// HealthChanged announces that the plugin health banners changed
	// (HealthBanners), for the console to read its manifest again.
	HealthChanged func()
	Log           *slog.Logger
}

// Bridge is the plugin host of one server.
type Bridge struct {
	cfg   Config
	log   *slog.Logger
	pkgs  *extpkg.Host
	state *pushstate.State
	host  *pluginhost.Host

	// admin serializes the administration calls (install, enable,
	// disable, uninstall) with each other and with Run's start.
	admin sync.Mutex

	mu sync.Mutex
	// entries are the installed packages as last verified, by id.
	entries map[string]extpkg.Entry
	// pushers are the identities of the running plugins, by package id,
	// bound when each is started.
	pushers map[string]identity
	// refusals are why an enabled package was not started (its data is
	// newer than its build, its data marker could not be read, or its host
	// setup is not applied), by package id, until it is started, stopped
	// or removed.
	refusals map[string]error
	// crashloops are the status params of each plugin that entered
	// crashloop and has not run since (latchCrashloop), by package id.
	crashloops map[string]map[string]any
	// running is true while Run runs: plugins enabled meanwhile start at
	// once.
	running bool
	// markerRetry is how long a start waits to be tried again when the
	// package's data marker could not be read.
	markerRetry time.Duration
	// known are the routes each installed package last verified with, by
	// package id, so one that breaks keeps answering them as down.
	known map[string]knownRoutes

	// routes is the route table compiled from entries on every refresh.
	routes atomic.Pointer[RouteTable]

	// health serializes refreshHealth; banners is its last judgement
	// (HealthBanners), held in memory only.
	health  sync.Mutex
	banners atomic.Pointer[[]pushstate.Banner]
}

// identity is who a plugin's pushes come from, and the data its build
// writes: bound when the plugin is started, dropped when it is stopped.
type identity struct {
	version string
	// onDemand marks a build whose server part starts on its first call:
	// its idle cycles are not reported as down and up.
	onDemand bool
	pusher   pushstate.Pusher
	// stateVersion is the build's storage.state_version, 0 without
	// storage.
	stateVersion int
	// marked is set once this start's first healthy run is recorded in
	// the package's data marker.
	marked bool
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
	if cfg.HostSetup.Dir == "" {
		cfg.HostSetup = hostsetup.DefaultRecords()
	}
	b := &Bridge{cfg: cfg, log: cfg.Log, pkgs: cfg.Packages, entries: map[string]extpkg.Entry{}, pushers: map[string]identity{}, refusals: map[string]error{}, crashloops: map[string]map[string]any{},
		markerRetry: 30 * time.Second, known: map[string]knownRoutes{}}
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
	applied := stateCfg.Effects.Applied
	stateCfg.Effects.Applied = func(owner, kind string, entries int) {
		if applied != nil {
			applied(owner, kind, entries)
		}
		b.countApplied(owner, kind, entries)
	}
	b.state = pushstate.New(stateCfg)
	if err := b.state.Load(ctx); err != nil {
		return nil, err
	}
	b.logFeatureClashes()
	b.declareFeatures()
	b.compileRoutesLocked()
	hostCfg := cfg.Host
	core := cfg.Core
	core.Offer = extpkg.Offer()
	if !coreVersionPattern.MatchString(core.Version) {
		core.Version = devCoreVersion
	}
	hostCfg.Core = func() extapi.CoreInfo { c := core; c.Offer = maps.Clone(core.Offer); return c }
	hostCfg.State = stateHost{b}
	hostCfg.ExtClock, hostCfg.Ready, hostCfg.OwnState = stateHost{b}, stateHost{b}, stateHost{b}
	if hostCfg.Clock == nil {
		hostCfg.Clock = func() extapi.Clock {
			r := b.state.Clocks()
			return extapi.Clock{Wall: extapi.NewTime(r.Wall), ExtClock: extapi.NewTime(r.ExtClock)}
		}
	}
	hostCfg.OnState = b.onState
	if hostCfg.Log == nil {
		hostCfg.Log = cfg.Log.With("component", "plugins")
	}
	if b.host, err = pluginhost.New(hostCfg); err != nil {
		return nil, err
	}
	b.refreshHealth()
	return b, nil
}

// State is the pushed state the server's hooks answer from.
func (b *Bridge) State() *pushstate.State { return b.state }

// Run runs pushed state's writer tick, timelines and capacity selection,
// starts every enabled resident plugin (an on-demand one waits for its
// first call), and stops them all when ctx ends.
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
// count as up, every other state as down (ADR 0071, decision 9). An
// on-demand plugin is not reported either way: it may not push state, and
// its idle cycles would otherwise write a down record each time.
func (b *Bridge) onState(st pluginhost.Status) {
	b.latchCrashloop(st)
	defer b.refreshHealth()
	b.mu.Lock()
	ident, bound := b.pushers[st.ID]
	b.mu.Unlock()
	if bound && ident.onDemand {
		if st.State == pluginhost.StateRunning {
			b.markData(st.ID)
		}
		return
	}
	if st.State.Up() {
		b.state.OwnerUp(st.ID, st.Generation)
		if st.State == pluginhost.StateRunning {
			b.markData(st.ID)
		}
		return
	}
	b.state.OwnerDown(st.ID)
}
