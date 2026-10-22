package expose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const namedFakeCloudflared = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo 'cloudflared version 2026.9.1 (fake)'
	exit 0
fi
while true; do sleep 1; done
`

// namedEnv puts a fake cloudflared on PATH, the way the plan asks tests to
// supply the connector.
func namedEnv(t *testing.T) *ProviderEnv {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudflared"), []byte(namedFakeCloudflared), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &ProviderEnv{Store: NewStore(t.TempDir()), Origin: "http://127.0.0.1:8800", Deps: NewDeps()}
}

func TestNamedValidate(t *testing.T) {
	store := NewStore(t.TempDir())
	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"no token file", &Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled}, "needs the tunnel token"},
		{"missing file", &Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled, TokenFile: filepath.Join(store.Root(), "nope")}, "not readable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&namedProvider{}).Validate(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}

	empty := filepath.Join(t.TempDir(), "empty-token")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&namedProvider{}).Validate(&Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled, TokenFile: empty}); err == nil {
		t.Fatal("an empty token file must be refused rather than sent to Cloudflare")
	}

	ok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(ok, []byte("eyJhIjoiZmFrZSJ9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&namedProvider{}).Validate(&Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled, TokenFile: ok}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestNamedLaunchKeepsTheTokenOutOfTheProcess is the security property the plan
// states: the token is a run credential, and it travels in a file the connector
// reads — never in a command line other processes can see, and never in the
// environment, which the connector's own children would inherit.
func TestNamedLaunchKeepsTheTokenOutOfTheProcess(t *testing.T) {
	env := namedEnv(t)
	token := "SECRET-TOKEN-VALUE-9f2c"
	tokenFile, err := env.Store.WriteToken(token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	// Readable by the owner and by the group of the credentials directory (the
	// CLI may run as root while the server runs as its service user), and by
	// nobody else.
	if perm := info.Mode().Perm(); perm != tokenMode {
		t.Errorf("token file mode = %v, want %v", perm, tokenMode)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("token file mode = %v must not be readable by anyone else", perm)
	}

	launch, err := (&namedProvider{}).Launch(context.Background(), env,
		&Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled, TokenFile: tokenFile}, Network{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	joined := strings.Join(launch.Args, " ")
	for _, want := range []string{"run", "--token-file", tokenFile, "--metrics", "--no-autoupdate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %v, want %q", launch.Args, want)
		}
	}
	if strings.Contains(joined, token) {
		t.Fatalf("the token must never be an argument: %v", launch.Args)
	}
	for _, kv := range launch.Env {
		if strings.Contains(kv, token) {
			t.Fatalf("the token must never be in the environment: %s", kv)
		}
	}
	// The connector's own configuration discovery is isolated from whatever the
	// service user has in their home directory.
	if !hasEnv(launch.Env, "HOME=") {
		t.Error("the connector must run with an isolated HOME")
	}
	for _, kv := range launch.Env {
		if strings.HasPrefix(kv, "TUNNEL_") || strings.HasPrefix(kv, "CF_") {
			t.Errorf("TUNNEL_*/CF_* must not be inherited: %s", kv)
		}
	}
}

func hasEnv(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// namedReadyFixture starts a fake configuration endpoint and returns a provider
// pointed at it.
func namedReadyFixture(t *testing.T, body string) (*namedProvider, *ProviderEnv) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	env := &ProviderEnv{Store: NewStore(t.TempDir()), Origin: "http://127.0.0.1:8800", Deps: NewDeps()}
	env.Deps.HTTP = srv.Client()
	return &namedProvider{metricsAddr: srv.Listener.Addr().String()}, env
}

func registered() StreamState {
	return StreamState{
		URL:          "https://named.example.com",
		Registration: &Registration{Edge: "lax01", Protocol: "quic"},
	}
}

func TestNamedReadyBeforeAnyRemoteConfiguration(t *testing.T) {
	p, env := namedReadyFixture(t, `{"version":-1,"config":{"ingress":[]}}`)
	ready, err := p.Ready(context.Background(), env, &Config{Revision: "r"}, registered())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready.State != StateAwaitingRoute {
		t.Fatalf("state = %q, want %q: an online connector with no configuration is not a published entry point", ready.State, StateAwaitingRoute)
	}
	if !strings.Contains(ready.Message, env.Origin) {
		t.Errorf("message %q should tell the operator which service to publish", ready.Message)
	}
}

func TestNamedReadyWithoutARoute(t *testing.T) {
	p, env := namedReadyFixture(t, `{"version":2,"config":{"ingress":[{"service":"http://localhost:9999"}]}}`)
	ready, err := p.Ready(context.Background(), env, &Config{Revision: "r"}, registered())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready.State != StateAwaitingRoute || ready.URL != "" {
		t.Fatalf("ready = %+v, want awaiting_route with no address", ready)
	}
}

func TestNamedReadySingleRoute(t *testing.T) {
	p, env := namedReadyFixture(t, `{"version":2,"config":{"ingress":[{"hostname":"gpu.example.com","service":"http://localhost:8800"}]}}`)
	ready, err := p.Ready(context.Background(), env, &Config{Revision: "r"}, registered())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready.State != StateReady || ready.URL != "https://gpu.example.com" {
		t.Fatalf("ready = %+v, want https://gpu.example.com", ready)
	}
	if !strings.Contains(ready.Message, "SSE") {
		t.Errorf("message %q should state what a named address supports", ready.Message)
	}
}

// TestNamedReadyAsksTheOperatorToChoose: picking one of several routes would
// hand out an address the operator never chose.
func TestNamedReadyAsksTheOperatorToChoose(t *testing.T) {
	p, env := namedReadyFixture(t, `{"version":2,"config":{"ingress":[
		{"hostname":"b.example.com","service":"http://localhost:8800"},
		{"hostname":"a.example.com","service":"http://localhost:8800"}]}}`)
	ready, err := p.Ready(context.Background(), env, &Config{Revision: "r"}, registered())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready.State != StateAwaitingRoute || len(ready.Candidates) != 2 {
		t.Fatalf("ready = %+v, want the two candidates offered for a choice", ready)
	}

	// The operator's choice is honoured, and only that.
	ready, err = p.Ready(context.Background(), env, &Config{Revision: "r", Hostname: "a.example.com"}, registered())
	if err != nil {
		t.Fatalf("Ready with a choice: %v", err)
	}
	if ready.State != StateReady || ready.URL != "https://a.example.com" {
		t.Fatalf("ready = %+v, want the chosen hostname", ready)
	}
}

// TestNamedReadyRevokesAChoiceThatIsGone: if the chosen route disappears from
// the tunnel, the entry point stops being ready rather than quietly serving a
// different site.
func TestNamedReadyRevokesAChoiceThatIsGone(t *testing.T) {
	p, env := namedReadyFixture(t, `{"version":2,"config":{"ingress":[{"hostname":"new.example.com","service":"http://localhost:8800"}]}}`)
	_, err := p.Ready(context.Background(), env, &Config{Revision: "r", Hostname: "old.example.com"}, registered())
	if err == nil {
		t.Fatal("a hostname the tunnel no longer publishes must not be reported as ready")
	}
	if !strings.Contains(err.Error(), "new.example.com") {
		t.Errorf("error %q should say what the tunnel publishes now", err)
	}
}

func TestNamedReadyNeedsARegistration(t *testing.T) {
	p, env := namedReadyFixture(t, `{"version":2,"config":{"ingress":[]}}`)
	if _, err := p.Ready(context.Background(), env, &Config{Revision: "r"}, StreamState{}); err == nil {
		t.Fatal("a connector that has not registered is not ready")
	}
}
