package testconn

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"syscall"
	"testing"
	"time"
)

// testNow is the fixed clock every hermetic runner reads.
var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// offlineRunner reaches nothing: the resolver answers a documentation
// address, every dial is refused and no proxy applies. A test replaces the
// seam it exercises.
func offlineRunner() *Runner {
	return &Runner{
		Now:        func() time.Time { return testNow },
		LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil },
		Dial:       func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED },
		Proxy:      func(*http.Request) (*url.URL, error) { return nil, nil },
		Getenv:     func(string) string { return "" },
	}
}

// dialTo sends every dial to a loopback test server, whatever address the
// runner asked for, and records the addresses asked.
func dialTo(server *httptest.Server, asked *[]string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if asked != nil {
			*asked = append(*asked, address)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, server.Listener.Addr().String())
	}
}

// dateServer answers every request with the given Date header; an empty date
// suppresses the header.
func dateServer(t *testing.T, date string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if date == "" {
			w.Header()["Date"] = nil
		} else {
			w.Header().Set("Date", date)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	return server
}

type runResult struct {
	checks  []Check
	summary Summary
	err     error
}

func run(ctx context.Context, r *Runner, in Input) runResult {
	var res runResult
	res.summary, res.err = r.Run(ctx, in, func(c Check) { res.checks = append(res.checks, c) })
	return res
}

func (res runResult) names() []string {
	names := make([]string, 0, len(res.checks))
	for _, c := range res.checks {
		names = append(names, c.Name)
	}
	return names
}

// check returns the named check, failing the test when it was not reported.
func (res runResult) check(t *testing.T, name string) Check {
	t.Helper()
	for _, c := range res.checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %s was not reported; got %v", name, res.names())
	return Check{}
}

// wire is the check exactly as a client receives it.
func wire(t *testing.T, c Check) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantWire(t *testing.T, c Check, want string) {
	t.Helper()
	if got := wire(t, c); got != want {
		t.Fatalf("%s wire frame\n got %s\nwant %s", c.Name, got, want)
	}
}

// captureLog collects what the default logger writes during the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}
