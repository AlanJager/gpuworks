package cardsmoke

import (
	"context"
	"strings"
	"testing"
	"time"
)

// waitReaping waits until the card's killed attempt is being waited for
// within its reap grace.
func waitReaping(t *testing.T, r *Runner, ident string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		reaping := r.rechecking[ident] && r.stuck[ident] > 0
		r.mu.Unlock()
		if reaping {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never entered its reap grace", ident)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitAttempts(t *testing.T, try *scripted, ident string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for try.count(ident) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s asked %d times, want %d", ident, try.count(ident), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// An on-demand check given CheckCardBudget (the RPC gives it that and the
// listing) has room for the whole re-check, even when the killed first
// attempt takes most of its reap grace to come back: the card's answer is the
// re-check's, timed out or not, and never "not checked".
func TestCheckCardUnderItsBudgetGetsTheRechecksAnswer(t *testing.T) {
	for name, wide := range map[string]step{"times out again": {slow: true}, "answers": {}} {
		t.Run(name, func(t *testing.T) {
			now, _ := clock(t)
			try := newScripted(map[string][]step{"a": {{slow: true, late: 80 * time.Millisecond}, wide}})
			t.Cleanup(func() { close(try.release) })
			r := New(Config{
				Cards:       func(context.Context) []Card { return cards("a") },
				Try:         try.try,
				Timeout:     20 * time.Millisecond,
				WideTimeout: 150 * time.Millisecond,
				ReapGrace:   100 * time.Millisecond,
				Now:         now,
			})
			// What the RPC adds for listing the cards, scaled down.
			ctx, cancel := context.WithTimeout(context.Background(), r.CheckCardBudget()+50*time.Millisecond)
			defer cancel()

			outcome, skip := r.CheckCard(ctx, "a")

			if skip != "" {
				t.Fatalf("skip = %q, want the card answered", skip)
			}
			if !outcome.Recheck.Made() || try.count("a") != 2 {
				t.Fatalf("outcome = %+v after %d attempts, want the re-check's answer", outcome, try.count("a"))
			}
			if wide.slow && (outcome.OK || !strings.Contains(outcome.Detail, "nor within 150ms")) {
				t.Fatalf("outcome = %+v, want both timeouts", outcome)
			}
			if !wide.slow && !outcome.OK {
				t.Fatalf("outcome = %+v, want the re-check's pass", outcome)
			}
		})
	}
}

// A caller's deadline that would end inside the reap grace is not waited
// into: the first timeout is the card's answer straight away. Waited into,
// the deadline would end the wait as a cancellation and the card would be
// reported as not checked.
func TestADeadlineInsideTheReapGraceKeepsTheFirstTimeout(t *testing.T) {
	now, _ := clock(t)
	try := newScripted(map[string][]step{"a": {{stuck: true}}})
	t.Cleanup(func() { close(try.release) })
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         try.try,
		Timeout:     20 * time.Millisecond,
		WideTimeout: 60 * time.Millisecond,
		ReapGrace:   time.Second,
		Now:         now,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	outcome, skip := r.CheckCard(ctx, "a")

	if skip != "" || outcome.OK || outcome.Recheck.Made() || !strings.Contains(outcome.Detail, "did not finish within 20ms") {
		t.Fatalf("outcome = %+v skip = %q, want the first timeout", outcome, skip)
	}
	if !strings.Contains(outcome.Detail, "not checked again") {
		t.Fatalf("detail = %q, want it to say there was no re-check", outcome.Detail)
	}
}

// While a killed attempt is within its reap grace, the card is being checked:
// another check asked about it then (CheckCard, CheckNow) is told so and
// records nothing, instead of a failure that would mark the card unusable
// until the re-check's answer replaced it.
func TestACheckDuringTheReapGraceIsInProgressNotAFailure(t *testing.T) {
	now, _ := clock(t)
	try := newScripted(map[string][]step{"a": {{stuck: true}, {}}})
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         try.try,
		Timeout:     20 * time.Millisecond,
		WideTimeout: 400 * time.Millisecond,
		ReapGrace:   5 * time.Second,
		Now:         now,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunOnce(context.Background())
	}()
	waitReaping(t, r, "a")

	outcome, skip := r.CheckCard(context.Background(), "a")
	if skip != SkipInProgress {
		t.Fatalf("CheckCard during the grace = %+v %q, want %q: the card's own check answers for it", outcome, skip, SkipInProgress)
	}
	r.CheckNow(context.Background(), map[string]bool{"a": true})
	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v during the grace, want nothing recorded", o)
	}
	if r.backingOff("a") {
		t.Fatal("a card in its reap grace earned a backoff")
	}

	close(try.release)
	<-done
	if a := r.Outcomes()["a"]; !a.OK || !a.Recheck.Made() || a.Failures != 0 {
		t.Fatalf("a = %+v, want the re-check's pass", a)
	}
	if got := try.count("a"); got != 2 {
		t.Fatalf("a asked %d times, want the attempt and the re-check only", got)
	}
}

// An agent shutting down while a killed attempt is within its reap grace is
// the round's end, not the card's: nothing is recorded and no backoff earned.
func TestCancelledDuringTheReapGraceRecordsNothing(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{stuck: true}, {}}})
	r := recheckRunner(t, try, "a")
	r.cfg.ReapGrace = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunOnce(ctx)
	}()
	waitReaping(t, r, "a")
	cancel()
	<-done

	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v, want nothing recorded for a round cancelled in the grace", o)
	}
	if r.backingOff("a") {
		t.Fatal("a card whose round was cancelled in the grace earned a backoff")
	}
	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times, want no re-check after the cancellation", got)
	}
}

// An agent shutting down during the re-check records nothing either.
func TestCancelledDuringTheRecheckRecordsNothing(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true}, {slow: true}}})
	r := recheckRunner(t, try, "a")
	r.cfg.WideTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunOnce(ctx)
	}()
	waitAttempts(t, try, "a", 2)
	cancel()
	<-done

	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v, want nothing recorded for a round cancelled in the re-check", o)
	}
	if r.backingOff("a") {
		t.Fatal("a card whose re-check was cancelled earned a backoff")
	}
}

// A first attempt that comes back only after its reap grace was stuck: the
// card is judged on the first timeout, no re-check starts after it returns,
// and once it has returned the card can be checked again.
func TestAnAttemptBackAfterTheGraceEarnsNoRecheck(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{stuck: true}, {}}})
	r := recheckRunner(t, try, "a")
	r.cfg.ReapGrace = 30 * time.Millisecond

	r.RunOnce(context.Background())

	a := r.Outcomes()["a"]
	if a.OK || a.Recheck.Made() || a.Failures != 1 || !strings.Contains(a.Detail, "did not finish within "+testTimeout.String()) {
		t.Fatalf("a = %+v, want the first timeout", a)
	}
	close(try.release)
	deadline := time.Now().Add(5 * time.Second)
	for r.anyStuck() {
		if time.Now().After(deadline) {
			t.Fatal("the attempt that came back late is still marked stuck")
		}
		time.Sleep(time.Millisecond)
	}
	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times, want no re-check once it came back late", got)
	}
	if outcome, skip := r.CheckCard(context.Background(), "a"); skip != "" || !outcome.OK || outcome.Recheck.Made() {
		t.Fatalf("CheckCard after it came back = %+v %q, want a plain pass", outcome, skip)
	}
}

// A check whose Try returns only after cleaning up for longer than the
// default reap grace (a container is stopped and removed first) earns the
// re-check when its ReapGrace covers the cleanup.
func TestAReapGraceCoveringTheCleanupEarnsTheRecheck(t *testing.T) {
	cleanup := 60 * time.Millisecond
	for name, tc := range map[string]struct {
		grace       time.Duration
		wantRecheck bool
	}{"covers the cleanup": {grace: 3 * cleanup, wantRecheck: true}, "shorter than the cleanup": {grace: cleanup / 3}} {
		t.Run(name, func(t *testing.T) {
			try := newScripted(map[string][]step{"a": {{slow: true, late: cleanup}, {}}})
			r := recheckRunner(t, try, "a")
			r.cfg.ReapGrace = tc.grace

			r.RunOnce(context.Background())

			if a := r.Outcomes()["a"]; a.Recheck.Made() != tc.wantRecheck || a.OK != tc.wantRecheck {
				t.Fatalf("a = %+v, want re-checked: %v", a, tc.wantRecheck)
			}
		})
	}
}

// CheckCardBudget is the first attempt, its reap grace, the wider re-check and
// the slack before the caller's deadline, or the attempt alone where no
// re-check is made.
func TestCheckCardBudgetCoversTheWholeRecheck(t *testing.T) {
	r := New(Config{Timeout: 75 * time.Second, ReapGrace: 35 * time.Second})
	if got, want := r.CheckCardBudget(), 75*time.Second+35*time.Second+225*time.Second+recheckSlack; got != want {
		t.Fatalf("CheckCardBudget = %s, want %s", got, want)
	}
	if got := New(Config{Timeout: time.Minute, WideTimeout: time.Second}).CheckCardBudget(); got != time.Minute {
		t.Fatalf("CheckCardBudget without a re-check = %s, want the attempt's minute", got)
	}
	var none *Runner
	if got := none.CheckCardBudget(); got != 0 {
		t.Fatalf("nil runner budget = %s", got)
	}
}
