package cardsmoke

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queuedTry makes the listed attempts (by card and attempt number) ones that spends its whole deadline waiting for another
// check's turn on the runtime and then gives up without starting, as the
// host's check does (gpu.RunTracked's ErrNoTurn, turned into Queued).
func queuedTry(inner *scripted, queuedOn map[string]map[int]bool) func(context.Context, Card) error {
	return func(ctx context.Context, card Card) error {
		n := inner.count(card.Ident)
		if queuedOn[card.Ident][n] {
			inner.mu.Lock()
			inner.attempts[card.Ident]++
			inner.mu.Unlock()
			<-ctx.Done()
			return Queued("another runtime check held the driver: " + ctx.Err().Error())
		}
		return inner.try(ctx, card)
	}
}

// An attempt that waited out its deadline for another check's turn never
// asked the card: it is no timeout, so nothing is recorded, no backoff is
// earned, no re-check is made, and the round goes on to the next card. The
// card is skipped as in progress.
func TestAnAttemptThatOnlyWaitedForItsTurnIsNotATimeout(t *testing.T) {
	now, _ := clock(t)
	inner := newScripted(nil)
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a", "b") },
		Try:         queuedTry(inner, map[string]map[int]bool{"a": {0: true}}),
		Timeout:     testTimeout,
		WideTimeout: testWideTimeout,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	r.RunOnce(context.Background())

	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v, want nothing recorded for an attempt that only waited", o)
	}
	if r.backingOff("a") {
		t.Fatal("an attempt that only waited earned a backoff")
	}
	if got := inner.count("a"); got != 1 {
		t.Fatalf("a asked %d times, want no re-check after a wait", got)
	}
	if skip := r.Skips()["a"]; skip.Reason != SkipInProgress {
		t.Fatalf("a skip = %+v, want %q", skip, SkipInProgress)
	}
	if b := r.Outcomes()["b"]; !b.OK {
		t.Fatalf("b = %+v, want the round to go on and ask it", b)
	}
	if r.Skipped() != "" {
		t.Fatalf("round skipped = %q, want it to have asked every card it could", r.Skipped())
	}
}

// A re-check that waited out the wide budget for another check's turn decides
// nothing either: the first timeout is not the card's verdict without the
// re-check, so nothing is recorded and an on-demand caller is told another
// check is in progress.
func TestARecheckThatOnlyWaitedForItsTurnRecordsNothing(t *testing.T) {
	now, _ := clock(t)
	inner := newScripted(map[string][]step{"a": {{slow: true}}})
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         queuedTry(inner, map[string]map[int]bool{"a": {1: true}}),
		Timeout:     testTimeout,
		WideTimeout: 60 * time.Millisecond,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	outcome, skip := r.CheckCard(context.Background(), "a")

	if skip != SkipInProgress {
		t.Fatalf("CheckCard = %+v %q, want %q", outcome, skip, SkipInProgress)
	}
	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v, want the first timeout not to stand without its re-check", o)
	}
	if r.backingOff("a") {
		t.Fatal("a re-check that only waited earned a backoff")
	}
	if got := inner.count("a"); got != 2 {
		t.Fatalf("a asked %d times, want the attempt and the re-check", got)
	}
}

// A first attempt the deadline decided before it came back, and that came back
// having only waited for its turn, is graded on what it said once back: no
// timeout, and no re-check.
func TestAWaitThatComesBackAfterTheDeadlineIsStillNoTimeout(t *testing.T) {
	now, _ := clock(t)
	inner := newScripted(nil)
	var asked atomic.Int32
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, card Card) error {
			if asked.Add(1) > 1 {
				return inner.try(ctx, card)
			}
			<-ctx.Done()
			// Long enough after the deadline that the runner has marked
			// the attempt stuck and is waiting for it within its grace.
			time.Sleep(30 * time.Millisecond)
			return Queued("another runtime check held the driver")
		},
		Timeout:     testTimeout,
		WideTimeout: testWideTimeout,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	r.RunOnce(context.Background())

	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v, want nothing recorded", o)
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("a asked %d times, want no re-check after a wait", got)
	}
	if skip := r.Skips()["a"]; skip.Reason != SkipInProgress {
		t.Fatalf("a skip = %+v, want %q", skip, SkipInProgress)
	}
}

// A card that passed only on the wider re-check is given the wide budget from
// the start next round, so a card that is slow but healthy does not time out,
// wait out the reap grace and get re-checked every round. Once it answers
// within the ordinary budget again it goes back to it.
func TestASlowCardStartsWithTheWideBudgetNextRound(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true}, {}, {}, {}}})
	r := recheckRunner(t, try, "a")

	r.RunOnce(context.Background())
	if a := r.Outcomes()["a"]; !a.OK || !a.Recheck.Made() {
		t.Fatalf("round 1: a = %+v, want the re-check's pass", a)
	}

	r.RunOnce(context.Background())
	budgets := try.budgets["a"]
	if len(budgets) != 3 || budgets[2] <= testTimeout {
		t.Fatalf("round 2: a budgets = %v, want one attempt with the wide budget", budgets)
	}
	if a := r.Outcomes()["a"]; !a.OK || a.Recheck.Made() {
		t.Fatalf("round 2: a = %+v, want a plain pass", a)
	}

	r.RunOnce(context.Background())
	if budgets := try.budgets["a"]; len(budgets) != 4 || budgets[3] > testTimeout {
		t.Fatalf("round 3: a budgets = %v, want the ordinary budget once the card answered within it", budgets)
	}
}

// A card that keeps needing more than the ordinary budget keeps the wide one:
// its pass on a wide first attempt took longer than Timeout.
func TestACardThatKeepsNeedingMoreKeepsTheWideBudget(t *testing.T) {
	now, _ := clock(t)
	var mu sync.Mutex
	var budgets []time.Duration
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, _ Card) error {
			deadline, _ := ctx.Deadline()
			budget := time.Until(deadline)
			mu.Lock()
			budgets = append(budgets, budget)
			mu.Unlock()
			if budget <= testTimeout {
				<-ctx.Done()
				return ctx.Err()
			}
			// Healthy, but slower than the ordinary budget every time.
			time.Sleep(2 * testTimeout)
			return nil
		},
		Timeout:     testTimeout,
		WideTimeout: testWideTimeout,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	for range 3 {
		r.RunOnce(context.Background())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 4 {
		t.Fatalf("budgets = %v, want one timed-out attempt and its re-check, then one wide attempt per round", budgets)
	}
	for i, b := range budgets[1:] {
		if b <= testTimeout {
			t.Fatalf("attempt %d budget = %s, want the wide budget", i+2, b)
		}
	}
	if a := r.Outcomes()["a"]; !a.OK || a.Failures != 0 {
		t.Fatalf("a = %+v, want a pass", a)
	}
}

// A card that fails after passing only on the re-check starts from the
// ordinary budget: only a pass carries the wide budget over.
func TestAFailureDropsTheWideBudget(t *testing.T) {
	refused := errors.New("cuInit=101: the driver refused the card")
	try := newScripted(map[string][]step{"a": {{slow: true}, {}, {err: refused}, {}}})
	r := recheckRunner(t, try, "a")

	r.RunOnce(context.Background())
	r.RunOnce(context.Background())
	if a := r.Outcomes()["a"]; a.OK || !strings.Contains(a.Detail, "refused") {
		t.Fatalf("round 2: a = %+v, want the definite failure", a)
	}
	// Past its backoff, as the next due round would be.
	r.mu.Lock()
	delete(r.nextAttempt, "a")
	r.mu.Unlock()
	r.RunOnce(context.Background())
	if budgets := try.budgets["a"]; len(budgets) != 4 || budgets[3] > testTimeout {
		t.Fatalf("a budgets = %v, want the ordinary budget after a failure", budgets)
	}
}
