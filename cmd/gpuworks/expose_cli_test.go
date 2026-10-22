package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The CLI's own behaviour is what these cover: what it asks, what it refuses,
// and what it is willing to call an address. The supervisor's side has its own
// tests; the assembly chain has the L1 case.

// terminalIO answers a scripted answer, so the interactive paths are testable
// without a pty.
func terminalIO(input string) (*selfinstall.IO, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	io := &selfinstall.IO{
		In:         strings.NewReader(input),
		Out:        out,
		Err:        errOut,
		IsTerminal: func() bool { return true },
	}
	return io, out, errOut
}

func TestChooseProvider(t *testing.T) {
	// One provider is offered, so there is nothing to ask — with or without a
	// terminal, and whether or not something was saved before.
	io, out, _ := terminalIO("")
	if got, err := chooseProvider(io, "", nil); err != nil || got != expose.ProviderQuick {
		t.Fatalf("chooseProvider = %q, %v; want the quick default", got, err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing to choose means nothing to ask, got:\n%s", out.String())
	}
	io.IsTerminal = func() bool { return false }
	if got, _ := chooseProvider(io, "", nil); got != expose.ProviderQuick {
		t.Fatalf("an unattended run = %q, want quick", got)
	}
	if got, _ := chooseProvider(io, expose.ProviderQuick, &expose.Config{Provider: expose.ProviderNamed}); got != expose.ProviderQuick {
		t.Fatalf("the flag wins = %q", got)
	}

	// The provider that is implemented but not offered says so, and points at
	// the one that works without any setup.
	_, err := chooseProvider(io, expose.ProviderNamed, nil)
	if err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("cloudflare-named must be refused with a reason, got %v", err)
	}
	if !strings.Contains(err.Error(), expose.ProviderQuick) {
		t.Errorf("the refusal should name the provider that is available: %v", err)
	}
	if _, err := chooseProvider(io, "ngrok", nil); err == nil {
		t.Fatal("an unknown provider must be refused")
	}
}

func TestChooseCredentialRules(t *testing.T) {
	store := expose.NewStore(t.TempDir())

	// quick never carries a credential.
	io, _, _ := terminalIO("")
	if got, err := chooseCredential(io, store, expose.ProviderQuick, "", nil); err != nil || got != "" {
		t.Fatalf("quick credential = %q (%v), want none", got, err)
	}

	// An unattended named run must be told where the token is: there is no
	// prompt to paste into.
	io, _, _ = terminalIO("")
	io.IsTerminal = func() bool { return false }
	if _, err := chooseCredential(io, store, expose.ProviderNamed, "", nil); err == nil {
		t.Fatal("named without a terminal and without --token-file must fail")
	}

	// --token-file is used as given: a file the operator owns is not copied.
	foreign := filepath.Join(t.TempDir(), "my-token")
	if err := os.WriteFile(foreign, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := chooseCredential(io, store, expose.ProviderNamed, foreign, nil); err != nil || got != foreign {
		t.Fatalf("credential = %q (%v), want the given file untouched", got, err)
	}

	// A pasted token is stored under the state directory, never echoed back and
	// never in the returned path's content.
	io, out, _ := terminalIO("sk-secret-token\n")
	path, err := chooseCredential(io, store, expose.ProviderNamed, "", nil)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if filepath.Dir(path) != store.CredentialsDir() {
		t.Fatalf("the pasted token should live in %s, got %s", store.CredentialsDir(), path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Owner and group (the state directory's group, which is the account the
	// server runs as when it is not the account that ran the CLI), and nobody
	// else.
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("token file mode = %v, want 0640", perm)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("token file mode = %v is readable by others", perm)
	}
	if strings.Contains(out.String(), "sk-secret-token") {
		t.Errorf("a pasted token must not be echoed:\n%s", out.String())
	}
}

// TestLiveStatusRefusesStaleAndOrphanedSnapshots is the rule --url-only rests
// on: neither a snapshot from another revision nor one whose server is gone is
// an address.
func TestLiveStatusRefusesStaleAndOrphanedSnapshots(t *testing.T) {
	store := expose.NewStore(t.TempDir())
	if err := store.WriteConfig(&expose.Config{Revision: "rev-1", Provider: expose.ProviderQuick, Desired: expose.DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteStatus(&expose.Status{Revision: "rev-old", State: expose.StateReady, URL: "https://stale.trycloudflare.com"}); err != nil {
		t.Fatal(err)
	}
	st, cfg := liveStatus(store)
	if cfg == nil {
		t.Fatal("the configuration should be read")
	}
	if st != nil && st.URL != "" {
		t.Fatalf("a snapshot from another revision must not be an address: %+v", st)
	}

	// The same revision, but no server holds the hosting lock: the entry point
	// went with the server that hosted it.
	if err := store.WriteStatus(&expose.Status{Revision: "rev-1", State: expose.StateReady, URL: "https://orphan.trycloudflare.com"}); err != nil {
		t.Fatal(err)
	}
	st, _ = liveStatus(store)
	if st == nil {
		t.Fatal("the observation should still be readable")
	}
	if st.URL != "" || st.State != expose.StateDisabled {
		t.Fatalf("without a hosting server the address must not survive: %+v", st)
	}
	if !strings.Contains(st.Message, "not running") {
		t.Errorf("message %q should say the server is not running", st.Message)
	}

	// A degraded address is not an address either: --url-only prints what a
	// script may dial, and exit 0 has to mean it works.
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := store.WriteStatus(&expose.Status{
		Revision: "rev-1", State: expose.StateDegraded, URL: "https://not-yet.trycloudflare.com",
		Message: "the connector is up but the address does not answer yet",
	}); err != nil {
		t.Fatal(err)
	}
	st, _ = liveStatus(store)
	if st == nil || st.State != expose.StateDegraded || st.URL == "" {
		t.Fatalf("the observation of a hosted entry point should be returned as it is: %+v", st)
	}
}

// TestExposeStoreQuietIsSilent: --url-only promises its stdout carries exactly
// one thing, the address.
func TestExposeStoreQuietIsSilent(t *testing.T) {
	io, out, _ := terminalIO("")
	store := exposeStore(t.TempDir(), io, true)
	if out.Len() != 0 {
		t.Fatalf("quiet discovery printed %q", out.String())
	}
	if store.Root() == "" {
		t.Fatal("the store should still be rooted")
	}
	io, out, _ = terminalIO("")
	exposeStore(t.TempDir(), io, false)
	if !strings.Contains(out.String(), "data directory:") {
		t.Errorf("the ordinary path should report the data directory, got %q", out.String())
	}
}

// TestWaitForReadyStatesWhatItWaitsFor: a request that has no address yet must
// end with the operator knowing the request is still active.
func TestWaitForReadyStatesWhatItWaitsFor(t *testing.T) {
	noServiceStartGrace(t)
	store := expose.NewStore(t.TempDir())
	io, _, errOut := terminalIO("")

	// Nothing has been observed for this revision, and no server hosts it: the
	// wait ends non-zero and says why rather than producing an address.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := waitForReady(ctx, store, io, "rev-1"); err == nil {
		t.Fatal("waiting without a published address must not succeed")
	}
	if !strings.Contains(errOut.String(), "the connector service is not running") {
		t.Errorf("the warning should say the connector service is not running, got %q", errOut.String())
	}
}

// TestWaitForReadyRefusesAnObservationNobodyHosts is the rule --url-only and
// `up` share: an address is only an address while a server is hosting the
// entry point it describes. A snapshot left behind by a server that is gone
// reads as a working address to anyone who trusts the file.
func TestWaitForReadyRefusesAnObservationNobodyHosts(t *testing.T) {
	noServiceStartGrace(t)
	store := expose.NewStore(t.TempDir())

	// A ready snapshot of the revision being asked about, written by a server
	// that is no longer running.
	if err := store.WriteStatus(&expose.Status{
		Revision: "rev-1", Provider: expose.ProviderQuick, State: expose.StateReady,
		URL: "https://stale.trycloudflare.com",
	}); err != nil {
		t.Fatal(err)
	}
	io, out, errOut := terminalIO("")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := waitForReady(ctx, store, io, "rev-1"); err == nil {
		t.Fatal("a snapshot nobody hosts must not be reported as the result")
	}
	if strings.Contains(out.String(), "stale.trycloudflare.com") {
		t.Errorf("the stale address must not be printed as an address:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "the connector service is not running") {
		t.Errorf("the operator should be told the connector service is not running, got %q", errOut.String())
	}
}

// TestWaitForReadyReportsAwaitingRoute: an entry point that is waiting for a
// route is a documented outcome, not a start failure — the connector is up and
// picks the route up once it exists.
func TestWaitForReadyReportsAwaitingRoute(t *testing.T) {
	store := expose.NewStore(t.TempDir())
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := store.WriteStatus(&expose.Status{
		Revision: "rev-1", Provider: expose.ProviderNamed, State: expose.StateAwaitingRoute,
		Message: "publish a route for http://127.0.0.1:8800",
	}); err != nil {
		t.Fatal(err)
	}
	io, out, _ := terminalIO("")
	if err := waitForReady(context.Background(), store, io, "rev-1"); !errors.Is(err, errNotServing) {
		t.Fatalf("err = %v, want errNotServing for awaiting_route", err)
	}
	if !strings.Contains(out.String(), "not published yet") || !strings.Contains(out.String(), "publish a route") {
		t.Errorf("the guidance should reach the operator, got:\n%s", out.String())
	}
}

// TestEntryPointInHand: `up` reuses the running entry point only while the
// observation says this server still has one. After the supervisor gave up,
// only a new revision lifts the failure cap — so a reused revision would make
// `up` a command that does nothing at all.
func TestEntryPointInHand(t *testing.T) {
	cfg := &expose.Config{Revision: "rev-1", Provider: expose.ProviderQuick, Desired: expose.DesiredEnabled}
	cases := []struct {
		name   string
		status *expose.Status
		want   bool
	}{
		{"ready", &expose.Status{Revision: "rev-1", State: expose.StateReady, URL: "https://x"}, true},
		{"starting", &expose.Status{Revision: "rev-1", State: expose.StateStarting}, true},
		{"degraded", &expose.Status{Revision: "rev-1", State: expose.StateDegraded, URL: "https://x"}, true},
		{"awaiting route", &expose.Status{Revision: "rev-1", State: expose.StateAwaitingRoute}, true},
		{"gave up", &expose.Status{Revision: "rev-1", State: expose.StateFailed, Message: "gave up after repeated attempts"}, false},
		{"disabled", &expose.Status{Revision: "rev-1", State: expose.StateDisabled}, false},
		{"another revision", &expose.Status{Revision: "rev-2", State: expose.StateReady, URL: "https://x"}, false},
		{"never observed", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryPointInHand(tc.status, cfg); got != tc.want {
				t.Fatalf("entryPointInHand = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestChooseRouteCommitsANewRevision: the answer is a new request, and the
// caller has to be told which one — waiting on the revision the answer replaced
// would report the state the operator just decided against.
func TestChooseRouteCommitsANewRevision(t *testing.T) {
	store := expose.NewStore(t.TempDir())
	cfg := &expose.Config{Revision: "rev-1", Provider: expose.ProviderNamed, Desired: expose.DesiredEnabled, TokenFile: "/tmp/token"}
	if err := store.WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	status := &expose.Status{Revision: "rev-1", Provider: expose.ProviderNamed, State: expose.StateAwaitingRoute,
		Candidates: []string{"a.example.com", "b.example.com"}}

	// A terminal is required: the choice is the operator's to make.
	io, _, errOut := terminalIO("")
	io.IsTerminal = func() bool { return false }
	if _, handled, err := chooseRoute(io, store, status); !handled || !errors.Is(err, errNotServing) {
		t.Fatalf("without a terminal: handled=%v err=%v, want handled with errNotServing", handled, err)
	}
	if !strings.Contains(errOut.String(), "nothing is picked automatically") {
		t.Errorf("the guidance should say nothing was chosen, got %q", errOut.String())
	}

	io, out, _ := terminalIO("2\n")
	revision, handled, err := chooseRoute(io, store, status)
	if handled || err != nil {
		t.Fatalf("choosing: handled=%v err=%v, want the wait to continue", handled, err)
	}
	if revision == "" || revision == "rev-1" {
		t.Fatalf("revision = %q, want a new one", revision)
	}
	written, err := store.ReadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if written.Revision != revision || written.Hostname != "b.example.com" {
		t.Fatalf("configuration = %+v, want the second candidate written as %q", written, revision)
	}
	if !strings.Contains(out.String(), "b.example.com") {
		t.Errorf("the operator should be told which address was chosen, got:\n%s", out.String())
	}
}

// noServiceStartGrace lets waitForReady say at once that no connector service
// is running, instead of giving a just-started one time to come up.
func noServiceStartGrace(t *testing.T) {
	t.Helper()
	old := serviceStartGrace
	serviceStartGrace = 0
	t.Cleanup(func() { serviceStartGrace = old })
}

// TestExposeRemoveKeepsConfigWhenTheLockIsUnexaminable: rm must not delete the
// configuration of an entry point it cannot prove is not being served.
func TestExposeRemoveKeepsConfigWhenTheLockIsUnexaminable(t *testing.T) {
	dir := t.TempDir()
	store := expose.NewStore(dir)
	if err := store.WriteConfig(&expose.Config{Revision: "rev-1", Provider: expose.ProviderQuick, Desired: expose.DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	// open on a directory fails whatever account runs the test, root included.
	if err := os.Mkdir(filepath.Join(store.Root(), ".supervisor.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := exposeRemove([]string{"-data", dir})
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether the connector service is running") {
		t.Fatalf("exposeRemove = %v, want a refusal that names the lock", err)
	}
	if _, err := store.ReadConfig(); err != nil {
		t.Fatalf("the configuration must survive the refusal: %v", err)
	}
}
