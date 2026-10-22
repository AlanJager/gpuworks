package expose

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These cover the two halves of the named flow that only the supervisor can
// show: a route published after the connector started has to be found while it
// waits, and a configuration nobody can read has to close the entry point.

// routeServer is a fake connector configuration endpoint whose body can change
// while the test runs: that is what publishing a route in the dashboard looks
// like from here.
type routeServer struct {
	srv *httptest.Server

	mu   sync.Mutex
	body string
	hits int
}

func newRouteServer(t *testing.T, body string) *routeServer {
	t.Helper()
	r := &routeServer{body: body}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.hits++
		body := r.body
		r.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *routeServer) publish(body string) {
	r.mu.Lock()
	r.body = body
	r.mu.Unlock()
}

func (r *routeServer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

func noRouteJSON() string {
	return `{"version":-1,"config":{"ingress":[]}}`
}

func oneRouteJSON(host string) string {
	return `{"version":3,"config":{"ingress":[{"hostname":"` + host + `","service":"http://localhost:8800"}]}}`
}

// namedHarness is a supervisor hosting a named entry point whose discovery
// endpoint is the fake above.
func namedHarness(t *testing.T, routes *routeServer) *supervisorHarness {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "tunnel-token")
	if err := os.WriteFile(tokenFile, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := metricsAddrFor
	metricsAddrFor = func(string) string { return routes.srv.Listener.Addr().String() }
	t.Cleanup(func() { metricsAddrFor = previous })

	// The script is the registering one: a named connector's address comes from
	// the remote configuration, not from a printed URL, but it still has to
	// attach to the edge before there is anything to discover.
	h := newSupervisorHarness(t, supervisorConnectorScript)
	if err := h.store.WriteConfig(&Config{
		Revision: "rev-named", Provider: ProviderNamed, Desired: DesiredEnabled, TokenFile: tokenFile,
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestSupervisorFindsARoutePublishedAfterItStarted is the documented named
// flow: only a token is given, and the route is published in the Cloudflare
// dashboard afterwards. The running entry point has to find it.
func TestSupervisorFindsARoutePublishedAfterItStarted(t *testing.T) {
	routes := newRouteServer(t, noRouteJSON())
	h := namedHarness(t, routes)
	h.sup.SetRouteInterval(50 * time.Millisecond)

	st := h.waitState(t, StateAwaitingRoute, 30*time.Second)
	if st.URL != "" {
		t.Fatalf("an entry point with no route must not carry an address: %+v", st)
	}

	routes.publish(oneRouteJSON("named.example.com"))

	// The promotion is a probe away: the supervisor has to reach ready with the
	// hostname the route names, without anyone running another command.
	st = h.waitState(t, StateReady, 30*time.Second)
	if st.URL != "https://named.example.com" {
		t.Fatalf("url = %q, want the route's hostname", st.URL)
	}
	if routes.count() < 2 {
		t.Fatalf("the configuration endpoint was read %d time(s): a route published later needs another read", routes.count())
	}
}

// TestSupervisorKeepsTheChoicesCurrentWhileItWaits: several routes are the
// operator's choice to make, and the list they are offered has to be the list
// that exists now.
func TestSupervisorKeepsTheChoicesCurrentWhileItWaits(t *testing.T) {
	routes := newRouteServer(t, oneRouteJSON("first.example.com"))
	h := namedHarness(t, routes)
	h.sup.SetRouteInterval(50 * time.Millisecond)

	// One route: ready, nothing to choose.
	st := h.waitState(t, StateReady, 30*time.Second)
	if st.URL != "https://first.example.com" {
		t.Fatalf("url = %q", st.URL)
	}
	// A second route appears: the supervisor must stop presenting a choice-less
	// address and ask, exactly as it would have at start-up.
	routes.publish(`{"version":4,"config":{"ingress":[
		{"hostname":"first.example.com","service":"http://localhost:8800"},
		{"hostname":"second.example.com","service":"http://localhost:8800"}]}}`)
	if st := h.waitState(t, StateAwaitingRoute, 30*time.Second); len(st.Candidates) != 2 {
		t.Fatalf("candidates = %v, want both routes offered", st.Candidates)
	}
}

// TestSupervisorClosesTheEntryPointWhenTheConfigurationIsUnreadable: the plan
// asks for a bad configuration to close the corresponding entry point, not to
// leave a connector publishing something nobody asked for any more.
func TestSupervisorClosesTheEntryPointWhenTheConfigurationIsUnreadable(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.enable(t, "rev-1")
	h.waitState(t, StateReady, 30*time.Second)
	pid := h.connectorPID(t)

	if err := os.WriteFile(h.store.ConfigPath(), []byte("{ this is not json"), 0o664); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "the connector to be taken down", func() bool { return !alive(pid) })
	// The observation is written after the child is gone, so a reader can look
	// in between: wait for the failure the reader is meant to see.
	waitFor(t, 30*time.Second, "the failure to be recorded", func() bool {
		st, _ := h.store.ReadStatus()
		return st != nil && st.State == StateFailed && st.URL == ""
	})
}

// TestSupervisorRecoversWhenANewRevisionFollowsGiveUp: giving up stops the
// attempts, so only a new request can lift it — and that request is `up`.
func TestSupervisorRecoversWhenANewRevisionFollowsGiveUp(t *testing.T) {
	h := newSupervisorHarness(t, supervisorCrashScript)
	h.sup.SetBackoff([]time.Duration{time.Millisecond, time.Millisecond})
	h.enable(t, "rev-1")

	st := h.waitState(t, StateFailed, 30*time.Second)
	// The first failure reaches StateFailed long before the last attempt gives
	// up, and each attempt runs a real child process, so allow the same margin
	// as the waits around it for a loaded runner.
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(st.Message, "gave up") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		st, _ = h.store.ReadStatus()
	}
	if st == nil || !strings.Contains(st.Message, "gave up") {
		t.Fatalf("the supervisor never gave up: %+v", st)
	}

	// The operator fixes whatever was wrong and runs `up` again, which writes a
	// new revision. That request has to reach the connector: without the reset
	// the supervisor stays blocked for ever and the retry does nothing.
	before := h.startCount(t)
	if err := h.store.WriteConfig(&Config{Revision: "rev-2", Provider: ProviderQuick, Desired: DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "a new attempt after the new revision", func() bool {
		return h.startCount(t) > before
	})
}
