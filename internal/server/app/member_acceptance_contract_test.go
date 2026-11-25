//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The field acceptance drivers and the edge in front of a cluster read these
// exact texts: the journal lines to find the member that runs the controller
// and to tell whether a member's materials are proven, and the response a
// member without business gives. Changing one breaks them without any other
// test noticing, so each is pinned here to its literal.

type syncedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *syncedLog) count(text string) int { return strings.Count(l.String(), text) }

func awaitLog(t *testing.T, l *syncedLog, text string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.count(text) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("journal has %d lines %q, want %d:\n%s", l.count(text), text, want, l.String())
}

func TestControllerRoleChangesAreJournaledWithTheirLiteralText(t *testing.T) {
	const started = `level=INFO msg="starting controller on local database leader"`
	const stopped = `level=INFO msg="stopping controller after local scheduling role lost"`
	journal := &syncedLog{}
	log := slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(t.Context())
	var mu sync.Mutex
	leader, observeErr := false, error(nil)
	setRole := func(local bool, err error) {
		mu.Lock()
		leader, observeErr = local, err
		mu.Unlock()
	}
	observe := func(context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return leader, observeErr
	}
	var running sync.WaitGroup
	run := func(ctx context.Context) {
		running.Add(1)
		defer running.Done()
		<-ctx.Done()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseController(ctx, observe, run, log, time.Millisecond, time.Second, nil, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })

	// A follower runs no controller and journals neither line.
	time.Sleep(20 * time.Millisecond)
	if journal.count(started) != 0 || journal.count(stopped) != 0 {
		t.Fatalf("follower journaled a controller role:\n%s", journal.String())
	}
	setRole(true, nil)
	awaitLog(t, journal, started, 1)
	// Holding the role journals nothing more.
	time.Sleep(20 * time.Millisecond)
	if journal.count(started) != 1 || journal.count(stopped) != 0 {
		t.Fatalf("steady leader journaled again:\n%s", journal.String())
	}
	setRole(false, nil)
	awaitLog(t, journal, stopped, 1)
	running.Wait()
	// A failed observation is a lost role, not a held one.
	setRole(true, nil)
	awaitLog(t, journal, started, 2)
	setRole(false, errors.New("no leader"))
	awaitLog(t, journal, stopped, 2)
	running.Wait()
}

func TestMaterialProofResultIsJournaledWithItsLiteralText(t *testing.T) {
	for _, mode := range []struct {
		name  string
		app   *App
		ready string
	}{
		// Neither member has a store to prove against: the proof answers
		// without one, which is the not-ready line the stability gate reads.
		{"closed member without a store", &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true}}, "ready=false"},
		{"member awaiting migration", &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, legacyClosed: true}}, "ready=false"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			journal := &syncedLog{}
			mode.app.log = slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelDebug}))
			eligibility := newCandidateMaterialEligibility(mode.app)
			ready, err := eligibility.Reprove(t.Context())
			if ready || err != nil {
				t.Fatalf("proof without a store: ready=%v err=%v", ready, err)
			}
			line := journal.String()
			if !strings.Contains(line, `level=DEBUG msg="candidate material proof completed" `+mode.ready+" ") {
				t.Fatalf("proof line: %s", line)
			}
			if strings.Count(line, "candidate material proof completed") != 1 {
				t.Fatalf("one proof journaled more than once: %s", line)
			}
		})
	}
	// At the default level the line is absent: a member the acceptance gate
	// watches must journal at debug level.
	journal := &syncedLog{}
	a := &App{ha: &memberRuntime{}, log: slog.New(slog.NewTextHandler(journal, nil)), memberState: memberState{joinedClosed: true}}
	if _, err := newCandidateMaterialEligibility(a).Reprove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if journal.String() != "" {
		t.Fatalf("proof line at info level: %s", journal.String())
	}
}

func TestClosedMemberRefusesBusinessAndReadinessButAnswersHealth(t *testing.T) {
	const refusal = "member business service is not ready\n"
	business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			return
		}
		if r.URL.Path == "/api/v1/readyz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"status\":\"ready\"}\n"))
			return
		}
		_, _ = w.Write([]byte("business"))
	})
	request := func(h http.Handler, method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	refused := func(t *testing.T, h http.Handler) {
		t.Helper()
		for _, target := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/readyz"},
			{http.MethodGet, "/api/v1/settings"},
			{http.MethodPut, "/api/v1/settings/acceptance.key"},
			{http.MethodPost, "/api/v1/auth/login"},
			{http.MethodPost, "/v1/chat/completions"},
			{http.MethodGet, "/"},
			{http.MethodPost, "/healthz"},
		} {
			rec := request(h, target.method, target.path)
			if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != refusal || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s = %d %q headers=%v", target.method, target.path, rec.Code, rec.Body.String(), rec.Header())
			}
		}
		health := request(h, http.MethodGet, "/healthz")
		if health.Code != http.StatusOK || health.Body.Len() != 0 {
			t.Fatalf("GET /healthz = %d %q", health.Code, health.Body.String())
		}
	}

	t.Run("no business generation", func(t *testing.T) {
		gate := newBusinessHandlerGate()
		gate.requireMaterialEligibility()
		refused(t, gate)
	})
	t.Run("generation installed without a proof", func(t *testing.T) {
		gate := newBusinessHandlerGate()
		gate.requireMaterialEligibility()
		gate.setBusinessHandler(business)
		refused(t, gate)
	})
	t.Run("proof lost under a serving generation", func(t *testing.T) {
		gate := newBusinessHandlerGate()
		gate.requireMaterialEligibility()
		eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
		gate.setMaterialEligibility(eligibility)
		gate.setBusinessHandler(business)
		if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
			t.Fatalf("proof: ready=%v err=%v", ready, err)
		}
		if rec := request(gate, http.MethodGet, "/api/v1/readyz"); rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ready\"}\n" {
			t.Fatalf("proven member readiness = %d %q", rec.Code, rec.Body.String())
		}
		if rec := request(gate, http.MethodPut, "/api/v1/settings/acceptance.key"); rec.Code != http.StatusOK || rec.Body.String() != "business" {
			t.Fatalf("proven member business = %d %q", rec.Code, rec.Body.String())
		}
		eligibility.Invalidate(closeMaterialPass)
		refused(t, gate)
	})
	t.Run("generation withdrawn", func(t *testing.T) {
		gate := newBusinessHandlerGate()
		gate.setBusinessHandler(business)
		if rec := request(gate, http.MethodGet, "/api/v1/readyz"); rec.Code != http.StatusOK {
			t.Fatalf("open gate readiness = %d", rec.Code)
		}
		if _, err := gate.withdrawBusinessHandler(t.Context()); err != nil {
			t.Fatal(err)
		}
		refused(t, gate)
	})
	t.Run("member without a store", func(t *testing.T) {
		refused(t, memberServiceHandler(business, &App{}, nil))
	})
}
