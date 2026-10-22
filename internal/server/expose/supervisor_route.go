package expose

import (
	"context"
	"errors"
	"time"
)

// The second half of a named entry point: the route is published after the
// connector starts, and can change while it runs.

// refreshRouteAsync asks the provider again what the connector's remote
// configuration publishes, without blocking the configuration loop.
//
// This is the second half of the named flow: the connector is started with
// nothing but a token, and the route is published afterwards. A route that
// appears (or disappears, or multiplies) has to be noticed by the running
// entry point, not only by the next attempt — the plan's "keeps receiving
// configuration while it waits".
func (s *Supervisor) refreshRouteAsync(ctx context.Context) {
	s.mu.Lock()
	if s.checkingRoute {
		s.mu.Unlock()
		return
	}
	s.checkingRoute = true
	s.lastRoute = s.deps.Now()
	child := s.child
	output := s.output
	provider := s.provider
	revision := s.revision
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.checkingRoute = false
			s.mu.Unlock()
		}()
		if child == nil || output == nil || provider == nil {
			return
		}
		cfg, err := s.store.ReadConfig()
		if err != nil {
			return
		}
		s.mu.Lock()
		origin := s.originLocked()
		s.mu.Unlock()
		if origin == "" {
			return
		}
		routeCtx, cancel := context.WithTimeout(s.probeContext(), 30*time.Second)
		defer cancel()
		ready, err := provider.Ready(routeCtx, &ProviderEnv{Store: s.store, Origin: origin, Deps: s.deps, Logf: s.note}, cfg, output.Snapshot())
		if err != nil {
			if errors.Is(err, errRouteGone) {
				// The route this observation rests on is not there any more.
				// Keeping the address would mean serving a site the operator
				// never chose: take it back and wait for a route that exists.
				s.revokeRoute(routeCtx, err)
				return
			}
			// Anything else is a transient failure to read the connector's own
			// endpoint, which says nothing about the entry point: it keeps its
			// observation, and the next check tries again.
			s.note("cannot read the published routes yet: %v", err)
			return
		}
		if s.superseded(revision) {
			return
		}
		switch ready.State {
		case StateReady:
			s.adoptRoute(routeCtx, ready)
		case StateAwaitingRoute:
			s.waitForRoute(ready)
		}
	}()
}

// adoptRoute makes the address the remote configuration names the published
// one. Ready keeps its meaning here: the address really serves this server,
// which is a judgement only the public probe can make — a route that was just
// published is not an address until it answers.
func (s *Supervisor) adoptRoute(ctx context.Context, ready Ready) {
	s.mu.Lock()
	current := s.url
	state := s.state
	s.mu.Unlock()
	if state == StateReady && current == ready.URL {
		return // nothing changed
	}
	s.mu.Lock()
	origin := s.originLocked()
	s.mu.Unlock()
	if err := s.deps.Prober.CheckLocal(ctx, origin); err != nil {
		s.note("the route is published, but the local origin is not healthy: %v", err)
		return
	}
	if err := s.probeEventually(ctx, ready.URL); err != nil {
		s.note("the route is published, but %s does not answer yet: %v", ready.URL, err)
		return
	}
	now := s.deps.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateReady && s.state != StateAwaitingRoute && s.state != StateDegraded {
		return
	}
	changed := s.url != ready.URL
	s.state = StateReady
	s.url = ready.URL
	s.candidates = ready.Candidates
	s.message = ready.Message
	s.remote = ready.Remote
	s.readySince = now
	s.lastProbe = now
	s.writeStatusLocked()
	if changed {
		s.log.Info("public entry point is ready", "provider", s.providerID, "url", ready.URL)
	}
}

// revokeRoute takes back an address whose route has gone: the operator removed
// it, or repointed it at something else. The connector stays up — publishing a
// route again is all it takes — but nothing here is an address any more.
func (s *Supervisor) revokeRoute(ctx context.Context, cause error) {
	s.mu.Lock()
	if s.state != StateReady && s.state != StateDegraded && s.state != StateAwaitingRoute {
		s.mu.Unlock()
		return
	}
	s.state = StateAwaitingRoute
	s.url = ""
	s.candidates = nil
	s.message = cause.Error()
	s.readySince = time.Time{}
	s.writeStatusLocked()
	s.mu.Unlock()
	s.log.Warn("the published route is gone; the entry point is waiting for one that exists", "err", cause)
}

// waitForRoute records an answer that names no single address: no route yet, or
// several of them. An observation that claimed one is revoked — picking between
// several published routes would hand the operator an address they did not
// choose, which is the one judgement this must not make for them.
func (s *Supervisor) waitForRoute(ready Ready) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateReady || s.state == StateDegraded {
		s.state = StateAwaitingRoute
		s.url = ""
		s.readySince = time.Time{}
		s.candidates = ready.Candidates
		s.message = ready.Message
		s.remote = ready.Remote
		s.writeStatusLocked()
		s.log.Warn("the published routes no longer name one address; the entry point waits for the operator to choose")
		return
	}
	if s.state != StateAwaitingRoute {
		return
	}
	if equalStrings(s.candidates, ready.Candidates) && s.message == ready.Message {
		return
	}
	s.candidates = ready.Candidates
	s.message = ready.Message
	s.remote = ready.Remote
	s.writeStatusLocked()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
