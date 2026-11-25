//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialReplicationFailedCycleRetriesBeforeNormalTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), materialReproofInterval+time.Second)
	defer cancel()
	started := time.Now()
	if !waitForMaterialReplicationCycle(ctx, make(chan time.Time), time.Minute, true) {
		t.Fatal("failed complete cycle waited for the normal minute tick instead of retrying")
	}
	if elapsed := time.Since(started); elapsed < materialReproofInterval {
		t.Fatalf("failed cycle skipped its retry delay: %s", elapsed)
	}
}

func TestMaterialReplicationRepeatedFailuresUseShorterIntervalWithoutSpin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	const interval = 35 * time.Millisecond
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	started := time.Now()
	for range 3 {
		if !waitForMaterialReplicationCycle(ctx, tick, interval, true) {
			t.Fatal("failed cycle did not use the shorter configured interval")
		}
	}
	if elapsed := time.Since(started); elapsed < 3*interval {
		t.Fatalf("repeated failures spun on a pending normal tick: %s", elapsed)
	}
}

func TestMaterialReplicationSuccessfulCycleRetainsOriginalTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	tick := make(chan time.Time, 1)
	done := make(chan bool, 1)
	go func() { done <- waitForMaterialReplicationCycle(ctx, tick, 35*time.Millisecond, false) }()
	select {
	case ready := <-done:
		t.Fatalf("successful cycle invented a retry wakeup: %t", ready)
	case <-time.After(80 * time.Millisecond):
	}
	tick <- time.Now()
	select {
	case ready := <-done:
		if !ready {
			t.Fatal("normal tick did not schedule the next successful cycle")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestMaterialReplicationRetryWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- waitForMaterialReplicationCycle(ctx, make(chan time.Time), time.Minute, true) }()
	cancel()
	select {
	case ready := <-done:
		if ready {
			t.Fatal("canceled failure scheduled another cycle")
		}
	case <-time.After(time.Second):
		t.Fatal("retry wait did not stop on cancellation")
	}
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	if waitForMaterialReplicationCycle(ctx, tick, time.Minute, false) {
		t.Fatal("an already queued normal tick overrode cancellation")
	}
}

func TestMaterialReplicationPassClosesMemberOnlyOnVerdict(t *testing.T) {
	integrity := errors.New("material digest differs")
	for _, tc := range []struct {
		name   string
		err    error
		closes bool
	}{
		{"pass ran out of time", context.DeadlineExceeded, false},
		{"store call ran out of time", fmt.Errorf("index material inventory locally: %w", context.DeadlineExceeded), false},
		{"no database leader", fmt.Errorf("grant: %w", dqlitedriver.ErrNoAvailableLeader), false},
		{"peer unreachable", &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: syscall.ECONNREFUSED}, false},
		{"too few members answered for a majority", fmt.Errorf("publish: %w", store.ErrMaterialManifestNoMajority), false},
		{"changed bytes", integrity, true},
		{"timeout joined to changed bytes", errors.Join(context.DeadlineExceeded, integrity), true},
		{"admission rejected", fmt.Errorf("grant: %w", store.ErrClusterAdmissionRejected), true},
		{"connection lost with a commit outstanding", driver.ErrBadConn, false},
		{"deadline with a commit outstanding", fmt.Errorf("commit manifest: %w", context.DeadlineExceeded), false},
		{"lost connection joined to changed bytes", errors.Join(driver.ErrBadConn, integrity), true},
		{"cancellation", context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
			gate.setOnInvalidated(func(admissionCloseCause) { closed++ })
			if ready, err := gate.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			a := &App{memberState: memberState{materialEligibility: gate}}
			a.materialReplicationPassFailed(tc.err)
			if tc.closes && (closed != 1 || gate.Eligible()) {
				t.Fatalf("verdict left the member open: closed=%d eligible=%v", closed, gate.Eligible())
			}
			if !tc.closes && (closed != 0 || !gate.Eligible()) {
				t.Fatalf("undecided failure closed the member: closed=%d eligible=%v", closed, gate.Eligible())
			}
		})
	}
}

// A run of passes that end without completing is reported once after thirty
// seconds, then every five minutes, and its end is reported only if the run
// itself was.
func TestMaterialPendingNoticeReportsOnceThenAtALowRate(t *testing.T) {
	var n materialPendingNotice
	start := time.Unix(1_700_000_000, 0)
	reports := 0
	for elapsed := time.Duration(0); elapsed <= 11*time.Minute; elapsed += 2 * time.Second {
		report, since, _ := n.pending(start.Add(elapsed))
		if !since.Equal(start) {
			t.Fatalf("run began at %v, want %v", since, start)
		}
		if report {
			reports++
			if want := []time.Duration{30 * time.Second, 5*time.Minute + 30*time.Second, 10*time.Minute + 30*time.Second}[reports-1]; elapsed != want {
				t.Fatalf("report %d after %v, want %v", reports, elapsed, want)
			}
		}
	}
	if reports != 3 {
		t.Fatalf("%d reports in eleven minutes, want 3", reports)
	}
	if reported, since, passes := n.ended(); !reported || !since.Equal(start) || passes != 331 {
		t.Fatalf("end of a reported run: reported=%t since=%v passes=%d", reported, since, passes)
	}
	// A short run is never reported, and neither is its end.
	n.pending(start)
	n.pending(start.Add(10 * time.Second))
	if reported, _, passes := n.ended(); reported || passes != 2 {
		t.Fatalf("end of a short run: reported=%t passes=%d", reported, passes)
	}
	if reported, _, passes := n.ended(); reported || passes != 0 {
		t.Fatalf("end with no run: reported=%t passes=%d", reported, passes)
	}
}

// What the log shows: nothing above debug for a short run, one warning that
// names the members not answering once the run has lasted, one line when it
// ends; a verdict is reported at once and starts no run.
func TestMaterialReplicationPendingRunIsLoggedOnceAndCleared(t *testing.T) {
	var logged bytes.Buffer
	a := &App{log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	noMajority := fmt.Errorf("%w; not answering: member 3 at 10.0.0.3:9001: connection refused; member 2 at 10.0.0.2:9001: HTTP 503",
		store.ErrMaterialManifestNoMajority)
	a.materialReplicationPassFailed(noMajority)
	a.materialReplicationPassCompleted()
	if logged.Len() != 0 {
		t.Fatalf("a single pending pass was logged above debug: %s", logged.String())
	}
	a.materialReplicationPassFailed(noMajority)
	a.materialPending.mu.Lock()
	a.materialPending.since = time.Now().Add(-materialPendingReportAfter - time.Second)
	a.materialPending.mu.Unlock()
	for range 5 {
		a.materialReplicationPassFailed(noMajority)
	}
	out := logged.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "has not completed") ||
		!strings.Contains(out, "member 3 at 10.0.0.3:9001: connection refused") || !strings.Contains(out, "member 2 at 10.0.0.2:9001: HTTP 503") ||
		!strings.Contains(out, "passes=2") {
		t.Fatalf("a lasting run: %s", out)
	}
	logged.Reset()
	a.materialReplicationPassCompleted()
	if out := logged.String(); strings.Count(out, "level=INFO") != 1 || !strings.Contains(out, "completed again") || !strings.Contains(out, "passes=6") {
		t.Fatalf("the run's end: %s", out)
	}
	logged.Reset()
	a.materialReplicationPassCompleted()
	if logged.Len() != 0 {
		t.Fatalf("a completed pass with no run before it was logged: %s", logged.String())
	}
	a.materialReplicationPassFailed(errors.New("material digest differs"))
	if out := logged.String(); strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "closes_member=true") {
		t.Fatalf("a verdict: %s", out)
	}
	if reported, _, passes := a.materialPending.ended(); reported || passes != 0 {
		t.Fatalf("a verdict started a pending run: reported=%t passes=%d", reported, passes)
	}
}

// The error of a pass that found too few members verified names the members
// it skipped, and stays the same kind of error; any other error is untouched.
func TestNoMajorityErrorNamesTheMembersNotAnswering(t *testing.T) {
	long := errors.New("Post \"https://10.0.0.3:9443/x\":\n dial tcp 10.0.0.3:9443: connect: connection refused " + strings.Repeat("x", 300))
	silent := []string{
		silentMaterialMember(store.ClusterTransportMember{ID: 3, Address: "10.0.0.3:9001"}, long),
		silentMaterialMember(store.ClusterTransportMember{ID: 2, Address: "10.0.0.2:9001"}, errors.New("manifest receiver refused: HTTP 503")),
	}
	err := namingSilentMaterialMembers(store.ErrMaterialManifestNoMajority, silent)
	text := err.Error()
	if !errors.Is(err, store.ErrMaterialManifestNoMajority) || materialReplicationFailureInvalidates(err) {
		t.Fatalf("the named error changed its kind: %v", err)
	}
	if !strings.Contains(text, "member 3 at 10.0.0.3:9001: Post") || !strings.Contains(text, "member 2 at 10.0.0.2:9001: manifest receiver refused: HTTP 503") ||
		strings.Contains(text, "\n") || len(silent[0]) > 200 {
		t.Fatalf("named error: %q", text)
	}
	other := errors.New("material digest differs")
	if got := namingSilentMaterialMembers(other, silent); got != other {
		t.Fatalf("another error was changed: %v", got)
	}
	if got := namingSilentMaterialMembers(store.ErrMaterialManifestNoMajority, nil); got != store.ErrMaterialManifestNoMajority {
		t.Fatalf("no member was skipped, yet the error changed: %v", got)
	}
}
