package expose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/logging"
)

// Supervisor defaults. They are the product's own limits — the operator is
// asked for none of them.
const (
	// DefaultInterval is how often the configuration is re-read.
	DefaultInterval = 5 * time.Second
	// DefaultStartTimeout bounds one attempt, registration included.
	DefaultStartTimeout = 90 * time.Second
	// DefaultStableWindow is how long a ready entry point must hold before the
	// failure count is cleared: a connector that starts and dies immediately
	// must not look like a recovery.
	DefaultStableWindow = 60 * time.Second
	// DefaultProbeInterval is how often a ready entry point is re-checked.
	DefaultProbeInterval = 30 * time.Second
	// DefaultProbeWindow is how long a registered address is retried before it
	// is called degraded. A fresh quick-tunnel hostname is often not in DNS
	// yet: the same measurement run saw it resolve in nine seconds once and
	// answer NXDOMAIN another time, and failing there would throw away a
	// working connector — and with it the address, since every restart mints a
	// new one.
	DefaultProbeWindow = 90 * time.Second
	// DefaultRouteInterval is how often an entry point that is waiting for a
	// route re-reads the connector's remote configuration. The documented named
	// flow is to start the connector with nothing but a token and publish the
	// route afterwards, so the route appearing later has to be noticed without
	// anyone running another command.
	DefaultRouteInterval = 15 * time.Second
	// stopGrace is how long the whole connector tree gets to exit before it is
	// killed. It has to outlast the helper's own grace plus its kill wait
	// (connectorTermGrace + 5s), or a connector that ignores SIGTERM would be
	// reported as a failed stop even though it did go away.
	stopGrace = 25 * time.Second
)

// defaultBackoff spaces out restart attempts for an entry point that keeps
// failing.
var defaultBackoff = []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}

// Supervisor owns the public entry point of one server.
//
// It is the only thing that starts, watches and stops the connector. The CLI
// never touches a process: it commits a configuration and reads observations.
// That is what keeps a public address from outliving the server it points at,
// and what makes two processes racing for the same entry point impossible.
type Supervisor struct {
	store *Store
	deps  Deps
	log   *slog.Logger

	interval      time.Duration
	startTimeout  time.Duration
	stableWindow  time.Duration
	probeInterval time.Duration
	probeWindow   time.Duration
	routeInterval time.Duration
	backoff       []time.Duration
	maxFailures   int

	// listenAddr is the address the server actually bound. The connector must
	// point at what the server is really listening on, never at a configured
	// guess, and a wildcard bind has to be turned into something dialable.
	// The connector service has no listener of its own and takes the origin
	// from the configuration instead (origin).
	listenAddr string
	origin     string

	// exitWhenDisabled ends Run once nothing is asked for and nothing runs:
	// the connector service exists only to host an entry point.
	exitWhenDisabled bool

	// runCtx is the supervisor's own lifetime, used for work that must outlive
	// one attempt (the public probe of a connector that is already up).
	runCtx context.Context

	mu     sync.Mutex
	child  *Child
	output *ConnectorOutput
	// provider is the instance that launched the child this supervisor holds.
	// It carries per-attempt state — the named provider's discovery endpoint is
	// the port of the connector we started — so anything that asks the provider
	// a question later (the route refresh of a waiting entry point) has to ask
	// the same instance, never a fresh one.
	provider    Provider
	logWriter   *logging.RotatingWriter
	revision    string
	providerID  string
	state       string
	url         string
	candidates  []string
	message     string
	failures    int
	nextAttempt time.Time
	readySince  time.Time
	lastProbe   time.Time
	lastRoute   time.Time
	// remote is set while the current observation came from a provider's remote
	// configuration, which is what makes it worth re-reading: the route behind
	// it can change without anything local changing at all.
	remote        bool
	lastErr       string
	binary        string
	version       string
	edge          string
	protocol      string
	starting      bool
	probing       bool
	checkingRoute bool
	// cancelAttempt ends the attempt in flight: a new revision (or disabled)
	// must not wait for a slow registration or a probe window to finish before
	// it can take effect, and the attempt must not publish afterwards.
	cancelAttempt context.CancelFunc
	// gaveUp records that the give-up has been reported, so it is said once
	// per round of attempts rather than on every converge.
	// A new attempt or a new configuration clears it.
	gaveUp bool
}

// NewSupervisor wires a supervisor for one data dir.

func NewSupervisor(store *Store, deps Deps, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		store:         store,
		deps:          deps,
		log:           log,
		interval:      DefaultInterval,
		startTimeout:  DefaultStartTimeout,
		stableWindow:  DefaultStableWindow,
		probeInterval: DefaultProbeInterval,
		probeWindow:   DefaultProbeWindow,
		routeInterval: DefaultRouteInterval,
		backoff:       defaultBackoff,
		maxFailures:   len(defaultBackoff) + 2,
		state:         StateDisabled,
	}
}

// SetListenAddr tells the supervisor what the server bound, once the listener
// exists. Until then an entry point cannot be started: there is nothing to
// point it at.

func (s *Supervisor) SetListenAddr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listenAddr = addr
}

// SetInterval overrides the configuration poll period (tests and harnesses).

func (s *Supervisor) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interval = d
}

// SetBackoff overrides the restart ladder, so tests can reach the give-up path
// without waiting a minute per step.

func (s *Supervisor) SetBackoff(steps []time.Duration) {
	if len(steps) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backoff = steps
	s.maxFailures = len(steps) + 2
}

// SetStartTimeout shortens one attempt's registration budget (tests).

func (s *Supervisor) SetStartTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startTimeout = d
}

// SetProbeInterval shortens how often a ready address is re-checked (tests).
func (s *Supervisor) SetProbeInterval(d time.Duration) {
	if d > 0 {
		s.probeInterval = d
	}
}

// SetRouteInterval shortens how often a waiting entry point re-reads the
// remote configuration (tests).

func (s *Supervisor) SetRouteInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routeInterval = d
}

// SetProbeWindow shortens the registered-but-not-yet-reachable window (tests).

func (s *Supervisor) SetProbeWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeWindow = d
}

// SetStableWindow shortens the "stayed up long enough" window (tests).

func (s *Supervisor) SetStableWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stableWindow = d
}

// Run hosts the entry point until ctx is done, then takes the connector down.
// A second server for the same data dir does not host at all: it says why and
// returns, leaving the main server running.

func (s *Supervisor) Run(ctx context.Context) {
	release, err := s.store.SupervisorLock()
	if err != nil {
		s.log.Error("cannot take the hosting lock", "err", err)
		return
	}
	defer release()
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()

	defer s.closeLogWriter()

	// A previous server's ready snapshot says nothing about this one: the
	// connector it described went away with that server. Keep the failure
	// count of the same revision, so a restart does not silently lift the
	// give-up limit.
	s.invalidatePreviousRun()

	s.mu.Lock()
	interval := s.interval
	s.mu.Unlock()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.converge(ctx)
		if s.exitWhenDisabled && s.idle() {
			s.log.Info("the entry point is disabled; the connector service exits")
			return
		}
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-ticker.C:
		}
	}
}

// idle reports that the entry point is disabled and nothing is running or
// starting.
func (s *Supervisor) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateDisabled && s.child == nil && !s.starting
}

// invalidatePreviousRun rewrites the observation so nobody reads a stale ready
// as a working address.

func (s *Supervisor) invalidatePreviousRun() {
	st, _ := s.store.ReadStatus()
	cfg, err := s.store.ReadConfig()
	if err != nil && !errors.Is(err, ErrNoConfig) {
		s.log.Warn("cannot read the exposure configuration", "err", err)
	}
	if st == nil {
		return
	}
	if cfg != nil && st.Revision == cfg.Revision {
		s.mu.Lock()
		s.failures = st.Failures
		s.mu.Unlock()
	}
	switch st.State {
	case StateReady, StateDegraded, StateStarting, StateAwaitingRoute:
		st.State = StateDisabled
		st.URL = ""
		st.Message = "the connector service restarted; the entry point will be re-established"
		if err := s.store.WriteStatus(st); err != nil {
			s.log.Warn("cannot record the invalidated observation", "err", err)
		}
	}
}

// converge is the whole decision surface: read what is wanted, and make what
// is running match it.

func (s *Supervisor) converge(ctx context.Context) {
	cfg, err := s.store.ReadConfig()
	if err != nil && !errors.Is(err, ErrNoConfig) {
		// A configuration nobody can read is not a reason to keep publishing:
		// the entry point that it described is no longer what anyone asked for.
		s.stopCurrent("the configuration cannot be read")
		s.recordFailure(fmt.Errorf("read the configuration: %w", err))
		return
	}
	if errors.Is(err, ErrNoConfig) {
		cfg = &Config{Provider: ProviderQuick, Desired: DesiredDisabled}
	}
	if cfg.Desired == DesiredDisabled {
		s.disable(cfg)
		return
	}
	provider, err := providerFor(cfg.Provider)
	if err != nil {
		s.stopCurrent("the configured provider is not one this server knows")
		s.recordFailure(err)
		return
	}
	if err := provider.Validate(cfg); err != nil {
		// The request is not usable as written — a token file that went away, a
		// provider that needs a credential it does not have. Whatever is
		// running was started from a configuration that no longer exists.
		s.stopCurrent("the configuration is not usable")
		s.recordFailure(err)
		return
	}

	s.mu.Lock()
	// A revision this supervisor has not seen is a fresh request, and the
	// failure budget belongs to the revision that spent it: without this, an
	// entry point that gave up could never be recovered — the plan's "until a
	// new revision's `up` restores it" needs the new revision to arrive and to
	// mean something.
	if s.revision != cfg.Revision {
		s.failures = 0
		s.gaveUp = false
		s.nextAttempt = time.Time{}
	}
	// A different revision or provider, or nothing running yet: the old
	// connector has to go before anything new starts. Switching providers is an
	// explicit act, and a gap while it happens is accepted.
	needStart := s.child == nil || s.revision != cfg.Revision || s.providerID != cfg.Provider
	blocked := s.failures >= s.maxFailures
	waitFor := s.nextAttempt
	inFlight := s.starting
	listen := s.listenAddr
	s.mu.Unlock()

	if needStart && s.currentChild() != nil {
		s.stopCurrent("the configuration changed")
	}
	if !needStart {
		s.watch(ctx)
		return
	}
	if blocked {
		s.giveUp()
		return
	}
	if inFlight || s.deps.Now().Before(waitFor) {
		return
	}
	origin := cfg.Origin
	if origin == "" {
		if listen == "" {
			s.recordFailure(errors.New("the configuration names no origin; run `gpuworks expose up` again"))
			return
		}
		if origin, err = OriginFromListenAddr(listen); err != nil {
			s.recordFailure(err)
			return
		}
	}
	s.mu.Lock()
	s.origin = origin
	s.mu.Unlock()
	s.launchAttempt(ctx, cfg, provider, origin)
}

// watch is the steady state: notice a connector that died, re-probe a ready
// address, and keep the failure count honest.

func (s *Supervisor) watch(ctx context.Context) {
	s.mu.Lock()
	child := s.child
	state := s.state
	lastProbe := s.lastProbe
	lastRoute := s.lastRoute
	remote := s.remote
	s.mu.Unlock()
	if child == nil {
		return
	}
	if child.Exited() {
		// The connector is gone: whatever address it published is not an address
		// any more, and the handle has to be dropped so the next converge starts
		// a new one instead of watching a corpse for ever.
		status := child.Wait()
		s.stopCurrent("the connector exited")
		s.recordFailure(fmt.Errorf("the connector exited: %v", status))
		return
	}
	if !child.HelperAlive() {
		// The connector is still running, but nothing is watching the server on
		// its behalf any more: the helper is what turns "the server is gone" into
		// "the connector stops". Left unmanaged it would keep serving this
		// server's address after the server is killed — the one outcome this
		// design exists to prevent — so take it down and start again under a
		// helper that is there.
		s.stopCurrent("the helper that owns the connector is gone")
		s.recordFailure(errors.New("the connector's helper exited; the connector was restarted"))
		return
	}
	// A degraded entry point is probed too: without that, losing reachability
	// once would mean never noticing it came back.
	if (state == StateReady || state == StateDegraded) && s.deps.Now().Sub(lastProbe) >= s.probeInterval {
		s.probeAsync(ctx)
	}
	// An address that came from the remote configuration is never settled: the
	// operator publishes the route in the Cloudflare dashboard *after* starting
	// the connector, and can repoint or remove it afterwards. Re-reading it is
	// what makes "fill in the token, then configure the route in the web UI" the
	// whole procedure, and what revokes an address whose route is gone.
	if remote && (state == StateReady || state == StateDegraded || state == StateAwaitingRoute) &&
		s.deps.Now().Sub(lastRoute) >= s.routeInterval {
		s.refreshRouteAsync(ctx)
	}
}

// launchAttempt starts one attempt in the background, so a slow download or a
// slow registration never blocks the configuration loop.

func (s *Supervisor) disable(cfg *Config) {
	// An attempt that is still downloading or probing has no child yet, and it
	// would hold the single worker slot until it finished on its own. The
	// operator asked for the entry point to be closed, so it ends here.
	s.stopCurrent("the entry point was disabled")
	s.mu.Lock()
	// A revision we have not recorded yet counts as a change even when the
	// state is already disabled: the CLI reads the revision to know that its
	// request was seen.
	changed := s.state != StateDisabled || s.url != "" || s.candidates != nil || s.revision != cfg.Revision
	s.state = StateDisabled
	s.url = ""
	s.candidates = nil
	s.remote = false
	s.message = ""
	s.lastErr = ""
	s.failures = 0
	s.gaveUp = false
	s.revision = cfg.Revision
	s.providerID = cfg.Provider
	if !changed {
		s.mu.Unlock()
		return
	}
	s.writeStatusLocked()
	s.mu.Unlock()
}

// giveUp stops trying until a new revision arrives, and says so once per round
// of attempts: the attempt's own reason stays in LastError, the guidance the
// operator needs goes in Message.
func (s *Supervisor) giveUp() {
	s.mu.Lock()
	if s.gaveUp {
		s.mu.Unlock()
		return
	}
	s.gaveUp = true
	s.state = StateFailed
	s.url = ""
	s.candidates = nil
	s.message = "gave up after repeated attempts; run `gpuworks expose up` to try again"
	s.writeStatusLocked()
	failures := s.failures
	s.mu.Unlock()
	s.log.Warn("giving up on the public entry point for now", "failures", failures)
}

// shutdown stops the connector and records that nothing is published. The
// configuration stays enabled: the next server start resumes it, and a named
// tunnel keeps its hostname while a quick one gets a new address.

func (s *Supervisor) shutdown() {
	if s.currentChild() != nil {
		s.stopCurrent("the server is shutting down")
	}
	s.mu.Lock()
	// A server that never had a configuration has nothing to report, and
	// writing an observation for it would touch the data directory on the way
	// out for no reason at all.
	hadEntryPoint := s.revision != "" || s.state != StateDisabled || s.url != ""
	s.state = StateDisabled
	s.url = ""
	s.message = "the connector service stopped; the entry point is closed with it"
	s.gaveUp = false
	if hadEntryPoint {
		s.writeStatusLocked()
	}
	s.mu.Unlock()
}

// recordFailure handles a failed attempt or a connector that died: backoff,
// and give up after enough tries.

func (s *Supervisor) recordFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
	}
	s.failures++
	s.state = StateFailed
	s.url = ""
	s.candidates = nil
	s.remote = false
	s.nextAttempt = s.deps.Now().Add(backoffFor(s.backoff, s.failures))
	s.log.Warn("public entry point attempt failed", "failures", s.failures, "err", s.lastErr)
	s.writeStatusLocked()
}

// note logs a step of an attempt for whoever reads the server log.

func (s *Supervisor) note(format string, args ...any) {
	s.log.Info("public entry point: " + fmt.Sprintf(format, args...))
}

func (s *Supervisor) currentChild() *Child {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.child
}

// stopCurrent stops the connector if one is running and forgets everything the
// attempt observed: the next attempt must not inherit a URL or a registration
// from the one that just failed.

func (s *Supervisor) stopCurrent(reason string) {
	s.stopCurrentWith(reason, true)
}

// stopFailedChild is stopCurrent for an attempt that is ending because it
// failed: the connector goes and the observation goes, but the attempt's own
// context is left alone. Cancelling it here would turn the reason this attempt
// failed into "context canceled" before anyone could read it, which is how a
// crash-looping connector came to be reported with no reason at all.
func (s *Supervisor) stopFailedChild(reason string) {
	s.stopCurrentWith(reason, false)
}

func (s *Supervisor) stopCurrentWith(reason string, cancelAttempt bool) {
	s.mu.Lock()
	child := s.child
	output := s.output
	cancel := s.cancelAttempt
	s.child = nil
	s.output = nil
	s.provider = nil
	s.cancelAttempt = nil
	s.url = ""
	s.remote = false
	s.mu.Unlock()
	if cancel != nil && cancelAttempt {
		// The attempt is over: whatever it is waiting for (a registration, a
		// probe window) is no longer wanted.
		cancel()
	}
	if output != nil {
		output.Reset()
	}
	if child == nil {
		return
	}
	if err := child.Stop(stopGrace); err != nil {
		s.log.Warn("cannot stop the connector cleanly", "reason", reason, "err", err)
	}
}

// probeAsync re-checks a ready address without blocking the configuration
// loop. A child that is alive but no longer reachable is degraded rather than
// failed: cloudflared reconnects on its own, and a brief DNS or network
// problem must not throw away an address the operator has already published.
