package conformance

import (
	"errors"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// undeclared are the server's refusals of a host call that needs a
// capability the handshake did not settle on, a permission the
// administrator did not approve or a role the certificate does not grant.
// The suite grants everything the manifest declares, so each means the
// plugin uses something it did not declare.
var undeclared = []string{codeCapabilityNotOffered, codeHostNotPermitted, codeRoleRequired}

// The server's host-call refusal codes, mirroring its
// ErrorPluginCapabilityNotOffered, ErrorPluginHostNotPermitted and
// ErrorPluginRoleRequired; this module cannot import the server.
const (
	codeCapabilityNotOffered = "PLUGIN_CAPABILITY_NOT_OFFERED"
	codeHostNotPermitted     = "PLUGIN_HOST_NOT_PERMITTED"
	codeRoleRequired         = "PLUGIN_ROLE_REQUIRED"
)

// checkInitialize: the answer names the package the manifest describes,
// as the server compares it with the installed package.
func (s *suite) checkInitialize(t *testing.T) {
	_, _, resp := s.initialize(t)
	for _, f := range []struct{ field, want, got string }{
		{"id", s.m.ID, resp.ID},
		{"version", s.m.Version, resp.Version},
		{"manifest_sha256", string(s.digest), string(resp.ManifestSHA256)},
	} {
		if f.want != f.got {
			t.Errorf("initialize answered %s %q; the package's is %q (the server refuses the binary with PLUGIN_HANDSHAKE_MISMATCH)",
				f.field, f.got, f.want)
		}
	}
}

// checkCapabilities: every capability the answer uses is one the manifest
// declares, at a version the offer contains and the manifest allows (what
// the server approves for the package); then, while the plugin runs, no
// host call is refused for a capability, permission or role it did not
// declare, and no push is of a table kind it did not negotiate.
func (s *suite) checkCapabilities(t *testing.T) {
	h, req, resp := s.initialize(t)
	approved := s.approved(req.Core.Offer)
	for _, name := range sortedKeys(resp.Use) {
		v := resp.Use[name]
		limit, declared := approved[name]
		switch {
		case !declared:
			t.Errorf("initialize uses %s@%d, which the manifest neither requires nor uses", name, v)
		case !req.Core.Offer[name].Contains(v) || v > limit:
			t.Errorf("initialize uses %s@%d; the server offers %d..%d and the manifest allows at most %d",
				name, v, req.Core.Offer[name].Min, req.Core.Offer[name].Max, limit)
		}
	}
	time.Sleep(s.cfg.PushWait)
	for _, c := range h.HostCalls() {
		if slices.Contains(undeclared, c.Code) {
			t.Errorf("%s %s was refused with %s: the plugin called the host API for something its manifest does not declare",
				c.Method, c.Path, c.Code)
		}
	}
	for _, p := range h.Pushes() {
		if capName := extapi.TableCapability(p.Envelope.Kind); resp.Use[capName] < 1 {
			t.Errorf("the plugin pushed %s without negotiating %s", p.Envelope.Kind, capName)
		}
	}
}

// approved is what the server lets the package use: each required
// capability at the version it offers, each optional one at most at the
// version the manifest names.
func (s *suite) approved(offer map[string]extapi.Range) map[string]int {
	out := map[string]int{}
	for name := range s.m.Requires {
		out[name] = offer[name].Max
	}
	for name, v := range s.m.Uses {
		if o, ok := offer[name]; ok {
			out[name] = min(v, o.Max)
		}
	}
	return out
}

// checkHealth: an initialized plugin answers health in time.
func (s *suite) checkHealth(t *testing.T) {
	h, _, _ := s.initialize(t)
	health, took, err := within(s.cfg.HealthTimeout, h.Health)
	switch {
	case errors.Is(err, errTimeout):
		t.Fatalf("health was not answered within %s; the server counts a miss and restarts the plugin after three in a row",
			s.cfg.HealthTimeout)
	case err != nil:
		t.Fatalf("health failed after %s: %v", took.Round(time.Millisecond), err)
	}
	if err := health.Validate(); err != nil {
		t.Fatalf("health answered %+v: %v", health, err)
	}
}

// checkShutdown: after shutdown the process exits with status 0 before
// the server would send SIGTERM.
func (s *suite) checkShutdown(t *testing.T) {
	h, _, _ := s.initialize(t)
	start := time.Now()
	err := h.ShutdownWithin(s.cfg.ShutdownTimeout)
	switch {
	case errors.Is(err, plugintest.ErrStillRunning):
		t.Fatalf("the process still runs %s after shutdown; the server would send SIGTERM", s.cfg.ShutdownTimeout)
	case err != nil:
		t.Fatalf("after shutdown the process ended with %v after %s; it should exit with status 0",
			err, time.Since(start).Round(time.Millisecond))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
