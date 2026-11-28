package cardsmoke

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// An on-demand check runs under its caller's deadline (Doctor's budget). A
// re-check that deadline would cut short decides nothing, so it is not
// started: the first timeout is the card's answer, as it was before re-checks.
func TestCheckCardUnderADeadlineTooShortForTheRecheckKeepsTheFirstTimeout(t *testing.T) {
	now, _ := clock(t)
	// Both slow: a re-check started anyway would be cut by the deadline,
	// recorded as nothing, and the card reported as not checked.
	try := newScripted(map[string][]step{"a": {{slow: true}, {slow: true}}})
	t.Cleanup(func() { close(try.release) })
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         try.try,
		Timeout:     50 * time.Millisecond,
		WideTimeout: 400 * time.Millisecond,
		ReapGrace:   20 * time.Millisecond,
		Now:         now,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	outcome, skip := r.CheckCard(ctx, "a")

	if skip != "" {
		t.Fatalf("skip = %q, want the card answered", skip)
	}
	if outcome.OK || outcome.Failures != 1 || outcome.Recheck.Made() || !strings.Contains(outcome.Detail, "did not finish within 50ms") {
		t.Fatalf("outcome = %+v, want the first timeout recorded", outcome)
	}
	// It says that the card was not checked again and why, so an operator
	// does not read it as a card that timed out twice.
	if !strings.Contains(outcome.Detail, "not checked again with 400ms, because its caller had only") {
		t.Fatalf("detail = %q, want why there was no re-check", outcome.Detail)
	}
	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times, want no re-check the deadline would cut short", got)
	}
}

// Once a card of a round has needed the wide budget, the rest of the round is
// given it from the start: eight slow cards must not each pay a timed-out
// attempt, the reap grace and a wide one. The next round gives it again only
// to the card that needed it; the cards that answered at once start from the
// ordinary budget.
func TestARoundThatNeededTheWideBudgetGivesItToTheRestOfTheRound(t *testing.T) {
	try := newScripted(map[string][]step{"a": {{slow: true}, {}}})
	r := recheckRunner(t, try, "a", "b", "c")

	r.RunOnce(context.Background())

	for _, ident := range []string{"b", "c"} {
		budgets := try.budgets[ident]
		if len(budgets) != 1 || budgets[0] <= testTimeout {
			t.Fatalf("%s budgets = %v, want one attempt with the wide budget", ident, budgets)
		}
		if o := r.Outcomes()[ident]; !o.OK || o.Recheck.Made() {
			t.Fatalf("%s = %+v, want a plain pass", ident, o)
		}
	}

	r.RunOnce(context.Background())
	if budgets := try.budgets["a"]; len(budgets) != 3 || budgets[2] <= testTimeout {
		t.Fatalf("a budgets = %v, want the next round to give the card that needed it the wide budget", budgets)
	}
	if budgets := try.budgets["b"]; len(budgets) != 2 || budgets[1] > testTimeout {
		t.Fatalf("b budgets = %v, want the next round to start from the ordinary budget", budgets)
	}
}

// A card taken by an instance while the re-check runs: the re-check's timeout
// is theirs, so nothing is recorded about the card.
func TestCardClaimedDuringTheRecheckRecordsNothing(t *testing.T) {
	now, _ := clock(t)
	try := newScripted(map[string][]step{"a": {{slow: true}, {slow: true}}})
	var mu sync.Mutex
	busy := false
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, card Card) error {
			if try.count(card.Ident) == 1 {
				mu.Lock()
				busy = true
				mu.Unlock()
			}
			return try.try(ctx, card)
		},
		Busy:        func(Card) bool { mu.Lock(); defer mu.Unlock(); return busy },
		Timeout:     testTimeout,
		WideTimeout: 60 * time.Millisecond,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	r.RunOnce(context.Background())

	if got := try.count("a"); got != 2 {
		t.Fatalf("a asked %d times, want the attempt and the re-check", got)
	}
	if o, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("a = %+v, want nothing recorded for a card taken during the re-check", o)
	}
	if r.backingOff("a") {
		t.Fatal("a card taken during the re-check earned a backoff")
	}
}

// Another check that became stuck while this card was asked (an on-demand
// check beside the round) holds the driver: no re-check joins it.
func TestAnotherStuckCheckBlocksTheRecheck(t *testing.T) {
	now, _ := clock(t)
	try := newScripted(map[string][]step{"a": {{slow: true}, {}}})
	var r *Runner
	r = New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, card Card) error {
			r.mu.Lock()
			r.stuck["z"] = time.Minute
			r.mu.Unlock()
			return try.try(ctx, card)
		},
		Timeout:     testTimeout,
		WideTimeout: testWideTimeout,
		ReapGrace:   testReapGrace,
		Now:         now,
	})

	r.RunOnce(context.Background())

	if got := try.count("a"); got != 1 {
		t.Fatalf("a asked %d times while another check is stuck, want once", got)
	}
	if a := r.Outcomes()["a"]; a.OK || a.Recheck.Made() || !strings.Contains(a.Detail, "did not finish within "+testTimeout.String()) {
		t.Fatalf("a = %+v, want the first timeout", a)
	}
}

// The reap grace defaults to five seconds, but never to more than the
// attempt's own budget.
func TestReapGraceDefaultsToTheShorterOfFiveSecondsAndTheBudget(t *testing.T) {
	if got := New(Config{}).cfg.ReapGrace; got != DefaultReapGrace {
		t.Fatalf("default ReapGrace = %s, want %s", got, DefaultReapGrace)
	}
	if got := New(Config{Timeout: 2 * time.Second}).cfg.ReapGrace; got != 2*time.Second {
		t.Fatalf("ReapGrace with a 2s budget = %s, want 2s", got)
	}
}
