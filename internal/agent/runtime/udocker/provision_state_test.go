package udocker

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// brokenRuntime is a driver whose directory cannot be created: its parent is
// a regular file, the failure a read-only or foreign mount produces too. The
// returned func repairs it.
func brokenRuntime(t *testing.T) (*Driver, *fakeClock, func()) {
	t.Helper()
	t.Setenv(launcherEnv, "runveil")
	base := t.TempDir()
	blocker := filepath.Join(base, "state")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(base)
	d.udockerDir = filepath.Join(blocker, "udocker")
	d.stateDir = filepath.Join(blocker, "udocker-state")
	clock := &fakeClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	d.track.now = clock.Now
	repair := func() {
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
	}
	return d, clock, repair
}

func TestFailedProvisioningIsServedFromMemoryUntilItsBackoffPasses(t *testing.T) {
	d, clock, repair := brokenRuntime(t)
	ctx := context.Background()

	first := d.ensureProvisioned(ctx)
	if first == nil || !errors.Is(first, runtime.ErrRuntimeUnavailable) || !strings.HasPrefix(first.Error(), "runveil runtime unavailable: ") {
		t.Fatalf("first failure = %v", first)
	}
	// The attempt is not repeated: repairing the node changes nothing until
	// the backoff passes, which proves the second call never ran the setup.
	repair()
	clock.Advance(provisionBackoffMin - time.Second)
	if err := d.ensureProvisioned(ctx); err != first {
		t.Fatalf("within the backoff got %v, want the remembered error", err)
	}
	if d.ProvisionState().Provisioned {
		t.Fatal("a remembered failure reports as provisioned")
	}

	// After the backoff the setup runs again, and a success clears the memory.
	clock.Advance(2 * time.Second)
	if err := d.ensureProvisioned(ctx); err != nil {
		t.Fatalf("retry after the repair failed: %v", err)
	}
	state := d.ProvisionState()
	if !state.Provisioned || state.LastError != "" || !state.LastErrorAt.IsZero() {
		t.Fatalf("state after success = %+v", state)
	}
	if err := d.ensureProvisioned(ctx); err != nil {
		t.Fatalf("provisioned runtime failed: %v", err)
	}
}

func TestProvisionBackoffDoublesToItsCeiling(t *testing.T) {
	d, clock, _ := brokenRuntime(t)
	ctx := context.Background()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, backoff := range want {
		failure := d.ensureProvisioned(ctx)
		if failure == nil {
			t.Fatalf("attempt %d succeeded", i)
		}
		clock.Advance(backoff - time.Second)
		if err := d.ensureProvisioned(ctx); err != failure {
			t.Fatalf("attempt %d: %v before %v elapsed, want the remembered error", i, err, backoff)
		}
		clock.Advance(time.Second)
	}
}

func TestProvisionStateReportsTheLastFailureWithoutRunningTheSetup(t *testing.T) {
	d, clock, _ := brokenRuntime(t)
	if state := d.ProvisionState(); state.Provisioned || state.LastError != "" {
		t.Fatalf("state before any attempt = %+v", state)
	}
	if _, err := os.Stat(d.udockerDir); err == nil {
		t.Fatal("reading the state created the runtime directory")
	}
	failedAt := clock.Now()
	if err := d.ensureProvisioned(context.Background()); err == nil {
		t.Fatal("setup of a broken runtime succeeded")
	}
	state := d.ProvisionState()
	if state.Provisioned || !strings.Contains(state.LastError, "not a directory") || !state.LastErrorAt.Equal(failedAt) {
		t.Fatalf("state after failure = %+v", state)
	}
	if strings.Contains(state.LastError, "runtime unavailable") {
		t.Fatalf("the cause carries the wrapper's words: %q", state.LastError)
	}
	if !strings.Contains(state.Probe, d.udockerDir) || !strings.Contains(state.Recheck, "findmnt -T "+d.udockerDir) {
		t.Fatalf("probe=%q recheck=%q", state.Probe, state.Recheck)
	}
}

func TestCancelledProvisioningIsNotRemembered(t *testing.T) {
	d, _, repair := brokenRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.ensureProvisioned(ctx); err == nil {
		t.Fatal("setup of a broken runtime succeeded")
	}
	if state := d.ProvisionState(); state.LastError != "" {
		t.Fatalf("a cancelled attempt was recorded: %+v", state)
	}
	repair()
	if err := d.ensureProvisioned(context.Background()); err != nil {
		t.Fatalf("the next caller was held back by the cancelled one: %v", err)
	}
}

func TestProvisionStateNamesTheOwnIdentityOfANonRootAgent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("covers the agent that is not root")
	}
	t.Setenv(launcherEnv, "runveil")
	d := New(t.TempDir())
	if err := d.ensureProvisioned(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := d.ProvisionState()
	if !state.Provisioned || state.RunUser != "" || state.RunUID != os.Geteuid() || state.FallbackReason != "" {
		t.Fatalf("state = %+v", state)
	}
}

func TestFallbackReasonWordsTheProbeFailure(t *testing.T) {
	if got := fallbackReason(errors.New("/ws/udocker refuses to change file ownership: operation not permitted\n")); got != "/ws/udocker refuses to change file ownership: operation not permitted" {
		t.Fatalf("reason = %q", got)
	}
	if got := fallbackReason(nil); got == "" {
		t.Fatal("a fallback without a probe error has no reason")
	}
}

func TestShellWordQuotesOnlyWhatNeedsIt(t *testing.T) {
	for in, want := range map[string]string{
		"/workspace/gpuworks/udocker": "/workspace/gpuworks/udocker",
		"/data/my dir":                "'/data/my dir'",
		"/data/it's":                  `'/data/it'\''s'`,
		"":                            "''",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// A root agent on a filesystem that refuses chown reports why it runs as
// itself.
func TestProvisionStateRecordsTheOwnershipFallback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dedicated runtime provisioning requires root")
	}
	if _, err := user.Lookup("udocker"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("GPUWORKS_TEST_STRICT=1 requires the udocker account: %v", err)
		}
		t.Skip("acceptance needs an existing udocker account; do not create test accounts")
	}
	t.Setenv(launcherEnv, "runveil")
	refuseOwnership(t)
	d := New(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.ensureProvisioned(ctx); err != nil {
		t.Fatal(err)
	}
	state := d.ProvisionState()
	if !state.Provisioned || state.RunUser != "" || state.RunUID != 0 ||
		!strings.Contains(state.FallbackReason, "refuses to change file ownership") {
		t.Fatalf("state = %+v", state)
	}
}
