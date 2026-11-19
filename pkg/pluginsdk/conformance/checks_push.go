package conformance

import (
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// observation is what a first run against an empty server pushed: the
// last applied table of each kind, as the server would hold it. Both push
// rules start from it, so it is taken once per Run.
type observation struct {
	once sync.Once
	held []extapi.HeldState
	err  string
}

// observe runs the binary against a server that holds nothing and returns
// the tables it pushed within the push wait. It skips t when the package
// is on_demand or pushed nothing, and fails t when the first run fails.
func (s *suite) observe(t *testing.T) []extapi.HeldState {
	t.Helper()
	if s.m.Runtime.Lifecycle == extapi.LifecycleOnDemand {
		t.Skip("an on_demand package pushes no table")
	}
	s.observed.once.Do(func() {
		var failed bool
		t.Run("first_run", func(t *testing.T) {
			h, _, _ := s.initialize(t)
			time.Sleep(s.cfg.PushWait)
			s.observed.held = appliedTables(h.Pushes())
			failed = t.Failed()
		})
		if failed {
			s.observed.err = "the first run, against a server holding nothing, failed"
		}
	})
	if s.observed.err != "" {
		t.Fatal(s.observed.err)
	}
	if len(s.observed.held) == 0 {
		t.Skipf("the plugin pushed no table within %s of initialize", s.cfg.PushWait)
	}
	return s.observed.held
}

// appliedTables is the last applied push of each kind, as held state.
func appliedTables(pushes []plugintest.Push) []extapi.HeldState {
	last := map[string]extapi.HeldState{}
	var order []string
	for _, p := range pushes {
		if p.Ack == nil {
			continue
		}
		if _, seen := last[p.Ack.Kind]; !seen {
			order = append(order, p.Ack.Kind)
		}
		last[p.Ack.Kind] = extapi.HeldState{Kind: p.Ack.Kind, Schema: p.Envelope.Schema, Generation: p.Ack.Generation, Digest: p.Ack.Digest}
	}
	out := make([]extapi.HeldState, 0, len(order))
	for _, kind := range order {
		out = append(out, last[kind])
	}
	return out
}

// checkUnchangedPush: restarted against a server that holds exactly what
// the first run pushed, the plugin sends none of those tables again.
func (s *suite) checkUnchangedPush(t *testing.T) {
	held := s.observe(t)
	h := s.start(t)
	h.SetHeld(held...)
	s.initializeStarted(t, h)
	time.Sleep(s.cfg.PushWait)
	byKind := map[string]extapi.HeldState{}
	for _, st := range held {
		byKind[st.Kind] = st
	}
	for _, p := range h.Pushes() {
		st, ok := byKind[p.Envelope.Kind]
		if !ok {
			continue
		}
		canonical, err := extapi.Canonical(p.Envelope.Body)
		if err != nil {
			continue
		}
		if extapi.DigestOf(canonical) == st.Digest {
			t.Errorf("the plugin pushed %s again at generation %d, although the server holds that table (digest %s) at generation %d",
				p.Envelope.Kind, p.Envelope.Generation, st.Digest, st.Generation)
		}
	}
}

// staleDigest stands for a table the plugin never pushed.
var staleDigest = extapi.DigestOf([]byte("conformance: a table this plugin never pushed"))

// checkChangedPush: restarted against a server that holds a different table
// of each kind, the plugin pushes each kind above the held generation and
// the push is applied.
func (s *suite) checkChangedPush(t *testing.T) {
	held := s.observe(t)
	stale := make([]extapi.HeldState, len(held))
	for i, st := range held {
		st.Digest = staleDigest
		stale[i] = st
	}
	h := s.start(t)
	h.SetHeld(stale...)
	s.initializeStarted(t, h)
	deadline := time.Now().Add(s.cfg.PushWait)
	for _, st := range stale {
		var applied, refused *plugintest.Push
		for applied == nil && time.Now().Before(deadline) {
			applied, refused = pushOf(h.Pushes(), st)
			if applied == nil {
				time.Sleep(10 * time.Millisecond)
			}
		}
		switch {
		case applied != nil:
		case refused != nil:
			t.Errorf("the plugin pushed %s at generation %d, refused with %s; the server holds generation %d",
				st.Kind, refused.Envelope.Generation, refused.Refusal.Code, st.Generation)
		default:
			t.Errorf("the plugin did not push %s within %s although the server holds a different table", st.Kind, s.cfg.PushWait)
		}
	}
}

// pushOf finds the first applied push of st's kind above its generation, or
// else the last refused one.
func pushOf(pushes []plugintest.Push, st extapi.HeldState) (applied, refused *plugintest.Push) {
	for i := range pushes {
		p := &pushes[i]
		if p.Envelope.Kind != st.Kind {
			continue
		}
		if p.Ack != nil && p.Ack.Generation > st.Generation {
			return p, nil
		}
		if p.Refusal != nil {
			refused = p
		}
	}
	return nil, refused
}

// checkOnDemand: an on_demand package pushes nothing; the server refuses
// its pushes with PLUGIN_STATE_ON_DEMAND.
func (s *suite) checkOnDemand(t *testing.T) {
	if s.m.Runtime.Lifecycle != extapi.LifecycleOnDemand {
		t.Skip("the package is resident")
	}
	h, _, _ := s.initialize(t)
	time.Sleep(s.cfg.PushWait)
	for _, p := range h.Pushes() {
		t.Errorf("the on_demand plugin pushed %s at generation %d", p.Envelope.Kind, p.Envelope.Generation)
	}
}
