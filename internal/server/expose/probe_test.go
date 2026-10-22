package expose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// proberFor points a prober at a test server, trusting its certificate exactly
// as production trusts a real one: the check under test must not be relaxed for
// the sake of the test.
func proberFor(t *testing.T, srv *httptest.Server) *Prober {
	t.Helper()
	client := srv.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = NewProber().Client.CheckRedirect
	return &Prober{Client: client, Timeout: 5 * time.Second}
}

func TestProberAcceptsTheHealthResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"dev","nodes":0,"sessions":0}`))
	}))
	defer srv.Close()

	p := proberFor(t, srv)
	if err := p.CheckPublic(context.Background(), srv.URL); err != nil {
		t.Fatalf("CheckPublic: %v", err)
	}
}

// TestProberRejectsSomethingElseAnswering is the readiness rule the plan spells
// out: a login page, a challenge or a proxy error answers 200 with HTML, and
// calling that ready would publish an address that does not serve this server.
func TestProberRejectsSomethingElseAnswering(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name: "html login page",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("<!doctype html><html><body>Sign in</body></html>"))
			},
			want: "not with this server's health response",
		},
		{
			name: "error status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bad gateway", http.StatusBadGateway)
			},
			want: "HTTP 502",
		},
		{
			name: "another json api",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"degraded"}`))
			},
			want: "not with this server's health response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(tc.handler)
			defer srv.Close()
			err := proberFor(t, srv).CheckPublic(context.Background(), srv.URL)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestProberRejectsPlainHTTPPublicAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	err := NewProber().CheckPublic(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("err = %v, want a refusal to treat a plain-HTTP address as a public entry point", err)
	}
}

// TestProberRejectsCrossHostRedirects: following a redirect to someone else
// proves that someone answers, not that this server does.
func TestProberRejectsCrossHostRedirects(t *testing.T) {
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer elsewhere.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/healthz", http.StatusFound)
	}))
	defer redirector.Close()

	p := proberFor(t, redirector)
	err := p.CheckPublic(context.Background(), redirector.URL)
	if err == nil {
		t.Fatal("a cross-host redirect must not count as the entry point answering")
	}
}

func TestProberReportsUnreachableAddress(t *testing.T) {
	// A closed port is what a dead connector looks like from the outside.
	err := NewProber().CheckPublic(context.Background(), "https://127.0.0.1:1")
	if err == nil {
		t.Fatal("want an error for an address nothing answers on")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error %q should say the address is not reachable", err)
	}
}

func TestProberCheckLocal(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	if err := proberFor(t, srv).CheckLocal(context.Background(), srv.URL); err != nil {
		t.Fatalf("CheckLocal: %v", err)
	}
	if err := proberFor(t, srv).CheckLocal(context.Background(), "https://127.0.0.1:1"); err == nil {
		t.Fatal("a dead local origin must fail")
	}
}
