// Package conformance checks a plugin binary against the rules the server
// holds every plugin to. A plugin author runs it from a test in the
// plugin's own module, against the binary that goes into the package:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, conformance.Config{
//			Binary:   "bin/linux_amd64/acme-backup",
//			Manifest: "manifest.json",
//		})
//	}
//
// Run starts the binary several times under plugintest's fake server, the
// way the server starts it (descriptors 3 and 4, the server's environment,
// no sandbox), and runs one subtest per rule, named after the rule (the
// Rule constants), so a failure names the rule that was broken. A rule
// that does not apply to the package (routes when it declares none, pushes
// for an on_demand package) is skipped.
package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// The rules, in the order Run checks them; each is a subtest's name.
const (
	// RuleInitialize: initialize is answered within the server's timeout,
	// with the id, version and manifest digest of the package.
	RuleInitialize = "initialize_answers_as_the_installed_package"
	// RuleDeclaredCapabilities: the handshake uses only capabilities the
	// manifest requires or uses, at versions the server offers and the
	// manifest allows, and no host call or push needs a capability,
	// permission or role the package did not declare.
	RuleDeclaredCapabilities = "uses_only_declared_capabilities"
	// RuleHealth: health is answered within the server's probe timeout.
	RuleHealth = "health_answers_within_the_timeout"
	// RuleShutdown: after shutdown the process exits with status 0 within
	// the server's grace, before it would be sent SIGTERM.
	RuleShutdown = "shutdown_exits_within_the_deadline"
	// RuleUnchangedPushSkipped: a table the server already holds with the
	// same digest is not pushed again.
	RuleUnchangedPushSkipped = "unchanged_push_is_skipped"
	// RuleChangedPushApplied: a table that differs from the one the server
	// holds is pushed above the held generation and applied.
	RuleChangedPushApplied = "changed_push_is_applied"
	// RuleOnDemandDoesNotPush: an on_demand package pushes no table.
	RuleOnDemandDoesNotPush = "on_demand_package_does_not_push"
	// RuleRouteServesValidPrincipal: a request the server signed for a
	// route is not refused for its principal.
	RuleRouteServesValidPrincipal = "routes_serve_a_valid_principal"
	// RuleRouteRefusesMissingPrincipal: a route request without a
	// principal is refused with 403.
	RuleRouteRefusesMissingPrincipal = "routes_refuse_a_missing_principal"
	// RuleRouteRefusesWrongMAC: a route request whose principal was signed
	// under another key is refused with 403.
	RuleRouteRefusesWrongMAC = "routes_refuse_a_wrong_mac"
	// RuleRouteRefusesOtherAction: a route request whose principal names
	// another route's action is refused with 403 (an action no route
	// declares when every route shares one action).
	RuleRouteRefusesOtherAction = "routes_refuse_another_routes_action"
)

// Rules returns every rule, in the order Run checks them.
func Rules() []string {
	return []string{RuleInitialize, RuleDeclaredCapabilities, RuleHealth, RuleShutdown,
		RuleUnchangedPushSkipped, RuleChangedPushApplied, RuleOnDemandDoesNotPush,
		RuleRouteServesValidPrincipal, RuleRouteRefusesMissingPrincipal, RuleRouteRefusesWrongMAC,
		RuleRouteRefusesOtherAction}
}

// The server's defaults for a plugin's lifecycle calls.
const (
	DefaultInitTimeout     = 10 * time.Second
	DefaultHealthTimeout   = 3 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
	DefaultPushWait        = 2 * time.Second
)

// Config names the binary under test and its package's manifest.
type Config struct {
	// Binary is the plugin executable; Args follow it ("serve" when
	// empty), as the server starts it.
	Binary string
	Args   []string
	// Manifest is the path of the package's manifest.json. Run reads the
	// package id, version, lifecycle, capabilities, permissions, roles and
	// routes from it, and its digest is what initialize must answer with.
	Manifest string
	// Env is added to the plugin's environment, for the author's own test
	// switches; the server adds nothing of the kind.
	Env []string
	// InitTimeout, HealthTimeout and ShutdownTimeout bound the lifecycle
	// calls; zero takes the server's defaults (10 s, 3 s and 10 s).
	InitTimeout     time.Duration
	HealthTimeout   time.Duration
	ShutdownTimeout time.Duration
	// PushWait is how long a run watches for pushes after initialize
	// (zero: 2 s). The push rules expect the plugin to push its tables
	// within it.
	PushWait time.Duration
}

// suite is one Run: the config and the manifest it names.
type suite struct {
	cfg      Config
	m        *extapi.Manifest
	digest   extapi.Digest
	observed observation
}

// Run checks the binary in cfg against every rule, each in a subtest named
// after it. A config it cannot use (a missing binary, a manifest that does
// not decode, a package without a server part) fails t before any rule.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	s, err := newSuite(cfg)
	if err != nil {
		t.Fatalf("conformance: %v", err)
	}
	checks := map[string]func(*testing.T){
		RuleInitialize:                   s.checkInitialize,
		RuleDeclaredCapabilities:         s.checkCapabilities,
		RuleHealth:                       s.checkHealth,
		RuleShutdown:                     s.checkShutdown,
		RuleUnchangedPushSkipped:         s.checkUnchangedPush,
		RuleChangedPushApplied:           s.checkChangedPush,
		RuleOnDemandDoesNotPush:          s.checkOnDemand,
		RuleRouteServesValidPrincipal:    s.checkRoutes(routeSigned),
		RuleRouteRefusesMissingPrincipal: s.checkRoutes(routeUnsigned),
		RuleRouteRefusesWrongMAC:         s.checkRoutes(routeWrongKey),
		RuleRouteRefusesOtherAction:      s.checkRoutes(routeOtherAction),
	}
	for _, rule := range Rules() {
		t.Run(rule, checks[rule])
	}
}

func newSuite(cfg Config) (*suite, error) {
	if st, err := os.Stat(cfg.Binary); err != nil {
		return nil, fmt.Errorf("the binary: %w", err)
	} else if st.IsDir() || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("the binary %s is not an executable file", cfg.Binary)
	}
	raw, err := os.ReadFile(cfg.Manifest)
	if err != nil {
		return nil, fmt.Errorf("the manifest: %w", err)
	}
	m, err := extapi.DecodeManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("the manifest %s: %w", cfg.Manifest, err)
	}
	if m.Runtime == nil {
		return nil, fmt.Errorf("the manifest %s declares no runtime: the package has no server part to check", cfg.Manifest)
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.InitTimeout, DefaultInitTimeout)
	def(&cfg.HealthTimeout, DefaultHealthTimeout)
	def(&cfg.ShutdownTimeout, DefaultShutdownTimeout)
	def(&cfg.PushWait, DefaultPushWait)
	return &suite{cfg: cfg, m: m, digest: extapi.DigestOf(raw)}, nil
}

// start runs the binary once under a fresh fake server; the plugin's log is
// added to t's output when t fails.
func (s *suite) start(t *testing.T) *plugintest.Harness {
	t.Helper()
	log := &lockedBuffer{}
	// Registered before the harness, so it runs after the harness closed
	// and the log is complete.
	t.Cleanup(func() {
		if t.Failed() {
			if out := log.String(); out != "" {
				t.Logf("the plugin's log:\n%s", out)
			}
		}
	})
	return plugintest.StartProcess(t, plugintest.Process{Path: s.cfg.Binary, Args: s.cfg.Args,
		ID: s.m.ID, Version: s.m.Version, Env: s.cfg.Env, Stderr: log})
}

// request is the harness's initialize request with what an administrator
// approves for the package: every permission it asks for and every role it
// claims.
func (s *suite) request(h *plugintest.Harness) *extapi.HandshakeRequest {
	req := h.Request()
	req.Plugin.PermissionsGranted = slices.Clone(s.m.Permissions)
	req.Plugin.RolesGranted = slices.Clone(s.m.Roles)
	return req
}

// initialize starts the binary and initializes it, failing t when it is
// refused or not answered within the init timeout.
func (s *suite) initialize(t *testing.T) (*plugintest.Harness, *extapi.HandshakeRequest, *extapi.HandshakeResponse) {
	t.Helper()
	h := s.start(t)
	return s.initializeStarted(t, h)
}

func (s *suite) initializeStarted(t *testing.T, h *plugintest.Harness) (*plugintest.Harness, *extapi.HandshakeRequest, *extapi.HandshakeResponse) {
	t.Helper()
	req := s.request(h)
	resp, took, err := within(s.cfg.InitTimeout, func() (*extapi.HandshakeResponse, error) { return h.Initialize(req) })
	switch {
	case errors.Is(err, errTimeout):
		t.Fatalf("initialize was not answered within %s", s.cfg.InitTimeout)
	case err != nil:
		t.Fatalf("initialize failed after %s: %v", took.Round(time.Millisecond), err)
	}
	return h, req, resp
}

// errTimeout is within's error for a call that did not return in time.
var errTimeout = errors.New("conformance: no answer in time")

// within runs f and returns its result, or errTimeout when it takes longer
// than d. A late f keeps running until the harness closes its connections.
func within[T any](d time.Duration, f func() (T, error)) (T, time.Duration, error) {
	type result struct {
		v   T
		err error
	}
	start := time.Now()
	done := make(chan result, 1)
	go func() {
		v, err := f()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, time.Since(start), r.err
	case <-time.After(d):
		var zero T
		return zero, d, errTimeout
	}
}

// lockedBuffer collects a plugin's log while the process writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimRight(b.buf.String(), "\n")
}
