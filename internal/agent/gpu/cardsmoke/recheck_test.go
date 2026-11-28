package cardsmoke

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTimeout     = 20 * time.Millisecond
	testWideTimeout = 400 * time.Millisecond
	testReapGrace   = 200 * time.Millisecond
)

// scripted is a Try that answers each attempt on a card in turn: an entry
// is either a definite answer, slow (returns when its deadline kills it, as a
// check whose process dies at the kill does), or stuck (ignores its context
// until released, as a process in D state does).
type scripted struct {
	mu       sync.Mutex
	steps    map[string][]step
	attempts map[string]int
	budgets  map[string][]time.Duration
	release  chan struct{}
}

type step struct {
	err   error
	slow  bool
	stuck bool
	// late is how long after its deadline a slow attempt comes back, as the
	// host's check does while it waits for the killed process to exit.
	late time.Duration
}

func (s *scripted) try(ctx context.Context, card Card) error {
	s.mu.Lock()
	n := s.attempts[card.Ident]
	s.attempts[card.Ident]++
	if deadline, ok := ctx.Deadline(); ok {
		s.budgets[card.Ident] = append(s.budgets[card.Ident], time.Until(deadline))
	}
	var st step
	if steps := s.steps[card.Ident]; n < len(steps) {
		st = steps[n]
	}
	s.mu.Unlock()
	switch {
	case st.stuck:
		<-s.release
		return nil
	case st.slow:
		<-ctx.Done()
		time.Sleep(st.late)
		return ctx.Err()
	}
	return st.err
}

func (s *scripted) count(ident string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[ident]
}

func newScripted(steps map[string][]step) *scripted {
	return &scripted{steps: steps, attempts: map[string]int{}, budgets: map[string][]time.Duration{}, release: make(chan struct{})}
}

func recheckRunner(t *testing.T, try *scripted, idents ...string) *Runner {
	t.Helper()
	now, _ := clock(t)
	t.Cleanup(func() {
		select {
		case <-try.release:
		default:
			close(try.release)
		}
	})
	return New(Config{
		Cards:       func(context.Context) []Card { return cards(idents...) },
		Try:         try.try,
		Timeout:     testTimeout,
		WideTimeout: testWideTimeout,
		ReapGrace:   testReapGrace,
		Now:         now,
	})
}

// A healthy card that is merely slow to answer — H20-3e ×8 cards (driver
// 580.126.09) ran past the 30 s budget — is asked once more with the wider
// budget, and its answer there is the card's: usable, no failure counted, no
// backoff, and the round goes on to the next card.
func TestTimedOutCardThatAnswersTheWiderRecheckIsUsable(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true}, {}}})
	r := recheckRunner(t, try, "a", "b")

	r.RunOnce(context.Background())

	a := r.Outcomes()["a"]
	if !a.OK || a.Failures != 0 || a.Detail != "" {
		t.Fatalf("a = %+v, want usable with no failure counted", a)
	}
	if want := (Recheck{FirstTimeout: testTimeout, Timeout: testWideTimeout}); a.Recheck != want {
		t.Fatalf("a.Recheck = %+v, want %+v", a.Recheck, want)
	}
	if got := try.count("a"); got != 2 {
		t.Fatalf("a asked %d times, want the attempt and one re-check", got)
	}
	if budgets := try.budgets["a"]; len(budgets) != 2 || budgets[0] > testTimeout || budgets[1] <= testTimeout {
		t.Fatalf("budgets = %v, want %s then the wider %s", budgets, testTimeout, testWideTimeout)
	}
	if r.backingOff("a") {
		t.Fatal("a card that answered the re-check is backing off")
	}
	if got := try.count("b"); got != 1 {
		t.Fatalf("b asked %d times, want the round to go on after a's re-check passed", got)
	}
	if b := r.Outcomes()["b"]; !b.OK || b.Recheck.Made() {
		t.Fatalf("b = %+v, want a plain pass", b)
	}
	if got := r.Skipped(); got != "" {
		t.Fatalf("skipped = %q, want none", got)
	}
}

// The kill and the deadline are the same instant, so a killed attempt comes
// back a moment after the runner has stopped waiting for it. One that comes
// back within the reap grace has left the driver and still earns the re-check.
func TestAttemptThatComesBackLateStillEarnsTheRecheck(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true, late: testReapGrace / 4}, {}}})
	r := recheckRunner(t, try, "a")

	r.RunOnce(context.Background())

	if a := r.Outcomes()["a"]; !a.OK || !a.Recheck.Made() {
		t.Fatalf("a = %+v, want the re-check's pass", a)
	}
	if got := try.count("a"); got != 2 {
		t.Fatalf("a asked %d times, want the attempt and one re-check", got)
	}
}

// A card that times out on the wider re-check as well is unusable, once:
// one failure, whose detail names both budgets, and the round stops as it
// does after any timeout.
func TestCardThatTimesOutTwiceIsUnusableNamingBothBudgets(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true}, {slow: true}}})
	r := recheckRunner(t, try, "a", "b")

	r.RunOnce(context.Background())

	a := r.Outcomes()["a"]
	if a.OK || a.Failures != 1 {
		t.Fatalf("a = %+v, want one failure", a)
	}
	for _, budget := range []string{testTimeout.String(), testWideTimeout.String()} {
		if !strings.Contains(a.Detail, budget) {
			t.Fatalf("detail %q does not name the %s budget", a.Detail, budget)
		}
	}
	if !a.Recheck.Made() {
		t.Fatalf("a.Recheck = %+v, want the re-check recorded", a.Recheck)
	}
	if got := try.count("a"); got != 2 {
		t.Fatalf("a asked %d times, want exactly one re-check", got)
	}
	if !r.backingOff("a") {
		t.Fatal("a card that failed the re-check is not backing off")
	}
	if got := try.count("b"); got != 0 {
		t.Fatalf("b asked %d times, want the round stopped after a timeout", got)
	}
	if got := r.Skipped(); !strings.Contains(got, testWideTimeout.String()) {
		t.Fatalf("skipped = %q, want the re-check's timeout as the reason", got)
	}
}

// A definite answer is the card's: a runtime that says no is not asked again
// with more time.
func TestDefiniteFailureEarnsNoRecheck(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{err: errors.New("cuInit=101")}, {}}})
	r := recheckRunner(t, try, "a")

	r.RunOnce(context.Background())

	a := r.Outcomes()["a"]
	if a.OK || a.Failures != 1 || !strings.Contains(a.Detail, "cuInit=101") || a.Recheck.Made() {
		t.Fatalf("a = %+v, want the definite failure without a re-check", a)
	}
	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times, want once", got)
	}
}

// A first attempt that did not return is stuck in the driver: no re-check
// starts beside it, the card is failed as before, and no later round starts
// another check while it is stuck.
func TestStuckAttemptStartsNoRecheck(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{stuck: true}, {}}})
	r := recheckRunner(t, try, "a", "b")

	r.RunOnce(context.Background())

	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times while its first check is stuck, want once", got)
	}
	a := r.Outcomes()["a"]
	if a.OK || a.Recheck.Made() || !strings.Contains(a.Detail, testTimeout.String()) || strings.Contains(a.Detail, testWideTimeout.String()) {
		t.Fatalf("a = %+v, want the first timeout and no re-check", a)
	}
	r.mu.Lock()
	r.nextAttempt = map[string]time.Time{}
	r.mu.Unlock()
	r.RunOnce(context.Background())
	if a, b := try.count("a"), try.count("b"); a != 1 || b != 0 {
		t.Fatalf("attempts a=%d b=%d after another round, want none started while one is stuck", a, b)
	}
}

// A re-check that cannot reach the runtime (the killed first process is
// still on the driver) decides nothing: the first timeout stands, as it
// would have without the re-check.
func TestInconclusiveRecheckLeavesTheFirstTimeoutStanding(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true}, {err: Inconclusive("an earlier runtime check on this node was killed and has not exited")}}})
	r := recheckRunner(t, try, "a")

	r.RunOnce(context.Background())

	a := r.Outcomes()["a"]
	if a.OK || a.Failures != 1 || a.Recheck.Made() || !strings.Contains(a.Detail, "did not finish within "+testTimeout.String()) {
		t.Fatalf("a = %+v, want the first timeout standing", a)
	}
}

// A card an instance took while it was being asked timed out because of
// them: it is not asked again, and nothing is recorded.
func TestCardClaimedDuringTheFirstAttemptIsNotRechecked(t *testing.T) {
	now, _ := clock(t)
	try := newScripted(map[string][]step{"a": {{slow: true}, {}}})
	var mu sync.Mutex
	busy := false
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, card Card) error {
			mu.Lock()
			busy = true
			mu.Unlock()
			return try.try(ctx, card)
		},
		Busy:        func(Card) bool { mu.Lock(); defer mu.Unlock(); return busy },
		Timeout:     testTimeout,
		WideTimeout: testWideTimeout,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	r.RunOnce(context.Background())

	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times, want no re-check of a card taken meanwhile", got)
	}
	if _, ok := r.Outcomes()["a"]; ok {
		t.Fatal("a card taken while asked has an outcome")
	}
}

// The default re-check budget is WideTimeoutFactor times the attempt's, so
// a vendor that sets its own Timeout gets a proportionate re-check.
func TestWideTimeoutDefaultsToAFactorOfTimeout(t *testing.T) {
	r := New(Config{Timeout: 45 * time.Second})
	if got, want := r.cfg.WideTimeout, WideTimeoutFactor*45*time.Second; got != want {
		t.Fatalf("WideTimeout = %s, want %s", got, want)
	}
	if got, want := New(Config{}).cfg.WideTimeout, WideTimeoutFactor*DefaultTimeout; got != want {
		t.Fatalf("default WideTimeout = %s, want %s", got, want)
	}
}
