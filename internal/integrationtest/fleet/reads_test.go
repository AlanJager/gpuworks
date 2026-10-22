package fleet

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiscardRejectsHTTPAndBodyFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		truncated bool
	}{{"server error", 500, false}, {"not found", 404, false}, {"truncated body", 200, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.truncated {
					w.Header().Set("Content-Length", "20")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "bad")
			}))
			defer srv.Close()
			c := &Client{base: srv.URL, http: srv.Client()}
			if _, err := c.Discard("/api/v1/nodes"); err == nil {
				t.Fatal("failed HTTP read was counted as success")
			}
		})
	}
}

func TestReadVerdictRejectsSlowFailedOrMissingObservations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		durations []time.Duration
		failures  []string
		want      string
	}{
		{"slow", []time.Duration{time.Second, 30 * time.Second}, nil, "exceeded"},
		{"failed", []time.Duration{time.Second}, []string{"status 500"}, "failed"},
		{"missing", nil, nil, "samples"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &Latencies{}
			for _, d := range tc.durations {
				l.Add(d)
			}
			if got := strings.Join(readProblems("nodes", l, tc.failures, 1), " "); !strings.Contains(got, tc.want) {
				t.Fatalf("problems=%q", got)
			}
		})
	}
}

func TestListReadProbeChecksEveryRouteAndReportsHTTPFailures(t *testing.T) {
	requests := make(chan string, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instances" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
		requests <- r.URL.RequestURI()
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	probe := StartListReadProbe(c, "onboarding-500-to-1000", 3)
	t.Cleanup(func() { probe.Stop() })
	counts := map[string]int{}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		enough := len(counts) == 4
		for _, n := range counts {
			enough = enough && n >= 3
		}
		if enough {
			break
		}
		select {
		case path := <-requests:
			counts[path]++
		case <-timeout.C:
			t.Fatal("did not observe all list routes")
		}
	}
	lines, problems := probe.Stop()
	if len(lines) != 4 {
		t.Fatalf("missing route evidence: %v", lines)
	}
	joined := strings.Join(problems, " ")
	if !strings.Contains(joined, "/api/v1/instances") || !strings.Contains(joined, "status 500") {
		t.Fatalf("HTTP error hidden: %v", problems)
	}
	for _, path := range []string{"/api/v1/nodes:", "/api/v1/nodes?page=1&page_size=30:", "/api/v1/deployments:"} {
		if strings.Contains(joined, path) {
			t.Fatalf("healthy route rejected: %v", problems)
		}
	}
}

func TestListReadProbeAllowsHealthySlowStableSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			_, _ = io.WriteString(w, `{"items":[]}`)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	p := StartListReadProbe(c, "stable-1000", 3)
	t.Cleanup(func() { p.Stop() })
	if err := p.WaitSamples(); err != nil {
		t.Fatal(err)
	}
	_, problems := p.Stop()
	if len(problems) != 0 {
		t.Fatalf("healthy two-second responses rejected: %v", problems)
	}
	for _, latency := range p.latencies {
		if latency.Count() < 3 {
			t.Fatal("stable window stopped too early")
		}
	}
}

func TestListReadProbeShortOnboardingDrainsOnlyStartedSamples(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release; _, _ = io.WriteString(w, `{"items":[]}`) }))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	p := StartListReadProbe(c, "short-onboarding", 1)
	// The phase is over before its first responses finish; drain those requests
	// without starting new requests and labelling them as onboarding.
	p.stopOnce.Do(func() { close(p.stop) })
	close(release)
	_, problems := p.Stop()
	if len(problems) != 0 {
		t.Fatalf("short phase rejected: %v", problems)
	}
	for _, latency := range p.latencies {
		if latency.Count() != 1 {
			t.Fatalf("post-phase samples=%d", latency.Count())
		}
	}
}
