package cardsmoke

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAFailingCardIsAskedLessOftenAndAWorkingOneEveryRound(t *testing.T) {
	now, advance := clock(t)
	r := &recorder{now: now, fail: map[string]error{"bad": errors.New("no context")}}
	runner := New(Config{
		Cards:    func(context.Context) []Card { return cards("bad", "good") },
		Try:      r.try,
		Interval: 15 * time.Minute,
		Now:      now,
	})

	for round := 0; round < 7; round++ {
		if round > 0 {
			advance(15 * time.Minute)
		}
		runner.RunOnce(context.Background())
	}

	var bad, good int
	for _, ident := range r.idents() {
		switch ident {
		case "bad":
			bad++
		case "good":
			good++
		}
	}
	if good != 7 {
		t.Errorf("a card that answers was asked %d times in 7 rounds, want every round", good)
	}
	if bad != 3 {
		t.Errorf("a failing card was asked %d times in 7 rounds, want it spaced out to 3", bad)
	}
	if outcome := runner.Outcomes()["bad"]; outcome.OK || outcome.Failures != 3 {
		t.Errorf("failing card outcome = %+v, want three recorded failures", outcome)
	}
}

func TestACardThatRecoversIsAskedEveryRoundAgain(t *testing.T) {
	now, advance := clock(t)
	r := &recorder{now: now, fail: map[string]error{"card": errors.New("no context")}}
	runner := New(Config{
		Cards:    func(context.Context) []Card { return cards("card") },
		Try:      r.try,
		Interval: 15 * time.Minute,
		Now:      now,
	})
	for round := 0; round < 4; round++ {
		if round > 0 {
			advance(15 * time.Minute)
		}
		runner.RunOnce(context.Background())
	}
	if outcome := runner.Outcomes()["card"]; outcome.Failures != 3 {
		t.Fatalf("outcome before repair = %+v, want three failures", outcome)
	}

	r.mu.Lock()
	r.fail = nil
	r.mu.Unlock()
	advance(60 * time.Minute)
	runner.RunOnce(context.Background())
	if outcome := runner.Outcomes()["card"]; !outcome.OK || outcome.Failures != 0 {
		t.Fatalf("outcome after repair = %+v, want a clean answer", outcome)
	}
	before := len(r.idents())
	advance(15 * time.Minute)
	runner.RunOnce(context.Background())
	if got := len(r.idents()); got != before+1 {
		t.Fatalf("a recovered card was asked %d more times, want it back every round", got-before)
	}
}

func TestTheBackoffStopsGrowingAtItsCeiling(t *testing.T) {
	now, advance := clock(t)
	r := &recorder{now: now, fail: map[string]error{"card": errors.New("no context")}}
	runner := New(Config{
		Cards:            func(context.Context) []Card { return cards("card") },
		Try:              r.try,
		Interval:         15 * time.Minute,
		MaxBackoffRounds: 4,
		Now:              now,
	})
	for failures := 0; failures < 6; failures++ {
		if failures > 0 {
			advance(4 * 15 * time.Minute)
		}
		runner.RunOnce(context.Background())
	}
	if got := len(r.idents()); got != 6 {
		t.Fatalf("attempts at the ceiling = %d, want one per ceiling-length wait", got)
	}
	before := len(r.idents())
	advance(15 * time.Minute)
	runner.RunOnce(context.Background())
	if got := len(r.idents()); got != before {
		t.Fatalf("a card inside its backoff was asked again (%d attempts)", got)
	}
}

func TestALongRoundDoesNotStretchTheBackoff(t *testing.T) {
	now, advance := clock(t)
	r := &recorder{now: now, fail: map[string]error{"slow": errors.New("no context")}}
	runner := New(Config{
		Cards: func(context.Context) []Card { return cards("slow") },
		Try: func(context.Context, Card) error {
			advance(12 * time.Minute)
			return r.try(context.Background(), Card{Ident: "slow"})
		},
		Interval:         15 * time.Minute,
		MaxBackoffRounds: 1,
		Now:              now,
	})
	runner.RunOnce(context.Background())
	if got := len(r.idents()); got != 1 {
		t.Fatalf("attempts in the first round = %d", got)
	}
	advance(3 * time.Minute)
	runner.RunOnce(context.Background())
	if got := len(r.idents()); got != 2 {
		t.Fatalf("attempts after the next tick = %d, want the card asked again", got)
	}
}

func TestAnInconclusiveAttemptRecordsNothingAndEndsTheRound(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"a": Inconclusive("libascendcl did not load")}, now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b", "c") },
		Try:   rec.try,
		Now:   now,
	})
	r.RunOnce(context.Background())

	if got := rec.idents(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("attempts = %v, want the round to stop at the first card", got)
	}
	if outcomes := r.Outcomes(); len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want a card that was asked but not judged", outcomes)
	}
	if got := r.Skipped(); !strings.Contains(got, "libascendcl") {
		t.Fatalf("skipped = %q, want the reason the round stopped", got)
	}
}

func TestAnInconclusiveAttemptLeavesAnEarlierVerdictAndItsBackoffAlone(t *testing.T) {
	now, advance := clock(t)
	rec := &recorder{fail: map[string]error{"a": errors.New("aclrtSetDevice=507033")}, now: now}
	r := New(Config{
		Cards:    func(context.Context) []Card { return cards("a") },
		Try:      rec.try,
		Now:      now,
		Interval: time.Minute,
	})
	r.RunOnce(context.Background())
	failed := r.Outcomes()["a"]
	if failed.OK || failed.Failures != 1 {
		t.Fatalf("first round = %+v, want one recorded failure", failed)
	}

	advance(2 * time.Minute)
	rec.mu.Lock()
	rec.fail["a"] = Inconclusive("the Ascend ACL runtime could not be used on this host")
	rec.mu.Unlock()
	r.RunOnce(context.Background())

	again := r.Outcomes()["a"]
	if again != failed {
		t.Fatalf("outcome = %+v, want the earlier verdict %+v", again, failed)
	}
	if got := r.Skipped(); !strings.Contains(got, "ACL runtime") {
		t.Fatalf("skipped = %q", got)
	}
}

func TestARoundResumesWhenTheRuntimeAnswersAgain(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"a": Inconclusive("libascendcl did not load")}, now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b") },
		Try:   rec.try,
		Now:   now,
	})
	r.RunOnce(context.Background())
	rec.mu.Lock()
	delete(rec.fail, "a")
	rec.mu.Unlock()
	r.RunOnce(context.Background())

	if got := r.Skipped(); got != "" {
		t.Fatalf("skipped = %q, want no reason once the check ran", got)
	}
	outcomes := r.Outcomes()
	if o, ok := outcomes["a"]; !ok || !o.OK {
		t.Fatalf("a = %+v", o)
	}
	if o, ok := outcomes["b"]; !ok || !o.OK {
		t.Fatalf("b = %+v, want the card behind it asked once the host answered", o)
	}
}

func TestDeadlineAndDoneCannotDisagreeAboutTimeout(t *testing.T) {
	for iteration := 0; iteration < 40; iteration++ {
		r := New(Config{
			Cards:   func(context.Context) []Card { return cards("a") },
			Try:     func(ctx context.Context, _ Card) error { <-ctx.Done(); return nil },
			Timeout: 2 * time.Millisecond,
		})
		r.RunOnce(context.Background())
		outcome := r.Outcomes()["a"]
		if outcome.OK || !strings.Contains(outcome.Detail, "did not finish") {
			t.Fatalf("iteration %d: outcome = %+v, want the same timeout whichever select branch wins", iteration, outcome)
		}

		// The attempt did return at the deadline, so after its goroutine has
		// finished publishing that return it must not remain stuck.
		deadline := time.Now().Add(time.Second)
		for {
			r.mu.Lock()
			stuck := r.stuck["a"]
			r.mu.Unlock()
			if !stuck {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: returned attempt remained stuck", iteration)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// The default ceiling is four intervals, about an hour: a card that keeps
// failing is not asked again for the whole window.
func TestTheDefaultBackoffCeilingIsFourIntervals(t *testing.T) {
	if DefaultMaxBackoffRounds != 4 {
		t.Fatalf("DefaultMaxBackoffRounds = %d, want 4", DefaultMaxBackoffRounds)
	}
	now, advance := clock(t)
	r := &recorder{now: now, fail: map[string]error{"card": errors.New("no context")}}
	runner := New(Config{
		Cards:    func(context.Context) []Card { return cards("card") },
		Try:      r.try,
		Interval: 15 * time.Minute,
		Now:      now,
	})
	// Failures 1..5 wait 1, 2, 4, 4, 4 intervals.
	for failures := 0; failures < 6; failures++ {
		if failures > 0 {
			wait := 1 << (failures - 1)
			if wait > 4 {
				wait = 4
			}
			advance(time.Duration(wait) * 15 * time.Minute)
		}
		runner.RunOnce(context.Background())
	}
	if got := len(r.idents()); got != 6 {
		t.Fatalf("attempts = %d, want one per capped wait", got)
	}
	before := len(r.idents())
	advance(3 * 15 * time.Minute)
	runner.RunOnce(context.Background())
	if got := len(r.idents()); got != before {
		t.Fatalf("a card was asked again %d intervals into a four-interval backoff", 3)
	}
	advance(15 * time.Minute)
	runner.RunOnce(context.Background())
	if got := len(r.idents()); got != before+1 {
		t.Fatalf("a card was not asked again after the four-interval ceiling (%d attempts)", got)
	}
}

// A shutdown that cancels the round while a check is in flight says nothing
// about the card: no outcome, no failure count, no backoff.
func TestACancelledAttemptIsDiscarded(t *testing.T) {
	now, _ := clock(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var tries int
	var mu sync.Mutex
	runner := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b") },
		Try: func(ctx context.Context, card Card) error {
			mu.Lock()
			tries++
			mu.Unlock()
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		Timeout:  time.Minute,
		Interval: 15 * time.Minute,
		Now:      now,
	})
	go func() {
		<-started
		cancel()
	}()
	runner.RunOnce(ctx)
	if outcomes := runner.Outcomes(); len(outcomes) != 0 {
		t.Fatalf("outcomes after a cancelled round = %+v, want none", outcomes)
	}
	if reason := runner.Skipped(); reason != "" {
		t.Fatalf("skipped = %q, want a cancelled round not reported as a skip reason", reason)
	}
	runner.mu.Lock()
	stuck, waiting := len(runner.stuck), len(runner.nextAttempt)
	runner.mu.Unlock()
	if stuck != 0 || waiting != 0 {
		t.Fatalf("cancelled attempt left stuck=%d backoff=%d", stuck, waiting)
	}
	mu.Lock()
	defer mu.Unlock()
	if tries != 1 {
		t.Fatalf("tries = %d, want the round to stop after the cancelled attempt", tries)
	}
}

// An attempt that ignores cancellation and returns its own failure while the
// round is being cancelled is also discarded, and the next round starts fresh.
func TestAFailureArrivingWithCancellationIsNotCountedAndNextRoundIsNotBackedOff(t *testing.T) {
	now, _ := clock(t)
	ctx, cancel := context.WithCancel(context.Background())
	runner := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(context.Context, Card) error {
			cancel()
			return errors.New("signal: killed")
		},
		Now: now,
	})
	runner.RunOnce(ctx)
	if outcomes := runner.Outcomes(); len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none", outcomes)
	}
	r := &recorder{now: now}
	runner.cfg.Try = r.try
	runner.RunOnce(context.Background())
	if got := r.idents(); len(got) != 1 {
		t.Fatalf("the round after a cancelled one asked %v, want the card tried at once", got)
	}
	if outcome := runner.Outcomes()["a"]; !outcome.OK || outcome.Failures != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
}
