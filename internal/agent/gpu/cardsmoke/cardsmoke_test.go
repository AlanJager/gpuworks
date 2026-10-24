package cardsmoke

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type attempt struct {
	ident string
	at    time.Time
}

// recorder is a Try that records what it was asked and answers as told.
type recorder struct {
	mu       sync.Mutex
	attempts []attempt
	fail     map[string]error
	block    chan struct{}
	now      func() time.Time
}

func (r *recorder) try(ctx context.Context, card Card) error {
	r.mu.Lock()
	r.attempts = append(r.attempts, attempt{ident: card.Ident, at: r.now()})
	block, err := r.block, r.fail[card.Ident]
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (r *recorder) idents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.attempts))
	for _, a := range r.attempts {
		out = append(out, a.ident)
	}
	return out
}

func cards(idents ...string) []Card {
	out := make([]Card, 0, len(idents))
	for _, ident := range idents {
		out = append(out, Card{Ident: ident, Visible: "GPU-" + ident})
	}
	return out
}

func clock(t *testing.T) (func() time.Time, func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		}
}

// Each card is asked separately: the answers differ, which is the whole
// reason for asking per card rather than once per node.
func TestEachCardIsAskedAndRemembersItsOwnAnswer(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"b": errors.New("cuInit=101")}, now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b", "c") },
		Try:   rec.try,
		Now:   now,
	})
	r.RunOnce(context.Background())

	if got := rec.idents(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("attempts = %v, want each card once in order", got)
	}
	outcomes := r.Outcomes()
	if o, ok := outcomes["a"]; !ok || !o.OK {
		t.Fatalf("a = %+v, want a card that answered", o)
	}
	if o, ok := outcomes["b"]; !ok || o.OK || o.Detail != "cuInit=101" {
		t.Fatalf("b = %+v, want the failure it reported", o)
	}
	if o, ok := outcomes["c"]; !ok || !o.OK {
		t.Fatalf("c = %+v", o)
	}
	if !outcomes["a"].At.Equal(now()) {
		t.Fatalf("the answer carries no time: %+v", outcomes["a"])
	}
}

// A round on a slow driver can outlast the interval. Starting another would
// put a second process on the same driver lock, which is the pile-up this
// exists to avoid.
func TestARoundNeverOverlapsAnother(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{block: make(chan struct{}), now: now}
	r := New(Config{Cards: func(context.Context) []Card { return cards("a", "b") }, Try: rec.try, Now: now})

	started := make(chan struct{})
	go func() {
		close(started)
		r.RunOnce(context.Background())
	}()
	<-started
	// Wait until the first card is in flight, then try to start a round.
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.idents()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first round never started")
		}
		time.Sleep(time.Millisecond)
	}
	r.RunOnce(context.Background())
	if got := len(rec.idents()); got != 1 {
		t.Fatalf("attempts while a round is in flight = %d, want the first card only", got)
	}
	close(rec.block)
}

// A card that is serving has answered by serving, and creating another
// context on it could disturb what runs there.
func TestACardThatIsServingIsLeftAlone(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a", "b") },
		Try:   rec.try,
		Busy:  func(card Card) bool { return card.Ident == "a" },
		Now:   now,
	})
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("attempts = %v, want only the idle card", got)
	}
	if _, recorded := r.Outcomes()["a"]; recorded {
		t.Fatal("a card that was not tried must not be recorded as anything")
	}
}

// A host where the check cannot run leaves every card unanswered rather
// than answered badly: "we could not ask" is not a verdict on any card.
func TestAHostThatCannotRunTheCheckAnswersForNoCard(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         rec.try,
		Unavailable: func() string { return "python3 is not installed" },
		Now:         now,
	})
	r.RunOnce(context.Background())
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts = %v, want none", got)
	}
	if outcome, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("outcome = %+v, want no answer recorded for an untried card", outcome)
	}
	// The reason is kept for doctor: a check that never ran must not read
	// as a check that passed.
	if got := r.Skipped(); got != "python3 is not installed" {
		t.Fatalf("skipped = %q", got)
	}
}

// What a card said while the check worked is not undone by the check later
// being unavailable.
func TestAnAnswerSurvivesTheCheckBecomingUnavailable(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{fail: map[string]error{"a": errors.New("cuInit=101")}, now: now}
	unavailable := ""
	r := New(Config{
		Cards:       func(context.Context) []Card { return cards("a") },
		Try:         rec.try,
		Unavailable: func() string { return unavailable },
		Now:         now,
	})
	r.RunOnce(context.Background())
	if o, ok := r.Outcomes()["a"]; !ok || o.OK {
		t.Fatalf("first round = %+v", o)
	}
	unavailable = "the accelerator driver is not answering"
	r.RunOnce(context.Background())
	if o, ok := r.Outcomes()["a"]; !ok || o.OK || o.Detail != "cuInit=101" {
		t.Fatalf("after the driver stopped answering = %+v, want the card's own answer kept", o)
	}
}

// Each attempt is bounded on its own, so one card that hangs cannot stall
// the round for the rest.
func TestAnAttemptIsBoundedOnItsOwn(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{block: make(chan struct{}), now: now}
	r := New(Config{
		Cards:   func(context.Context) []Card { return cards("a", "b") },
		Try:     rec.try,
		Timeout: 50 * time.Millisecond,
		Now:     now,
	})
	start := time.Now()
	r.RunOnce(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a round of two hanging cards took %s", elapsed)
	}
	if got := rec.idents(); len(got) != 2 {
		t.Fatalf("attempts = %v, want the hanging card not to stop the next", got)
	}
	for _, ident := range []string{"a", "b"} {
		if o, ok := r.Outcomes()[ident]; !ok || o.OK {
			t.Fatalf("%s = %+v, want a recorded failure", ident, o)
		}
	}
	close(rec.block)
}

// A cancelled context stops the round rather than working through the rest.
func TestACancelledRoundStops(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := New(Config{Cards: func(context.Context) []Card { return cards("a", "b") }, Try: rec.try, Now: now})
	r.RunOnce(ctx)
	if got := rec.idents(); len(got) != 0 {
		t.Fatalf("attempts after cancellation = %v", got)
	}
}

func TestRunDoesTheFirstRoundWithoutWaitingForTheInterval(t *testing.T) {
	now, _ := clock(t)
	rec := &recorder{now: now}
	r := New(Config{
		Cards:      func(context.Context) []Card { return cards("a") },
		Try:        rec.try,
		Interval:   time.Hour,
		FirstRound: time.Millisecond,
		Now:        now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.idents()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a node that just connected waited for the interval to learn its cards cannot be used")
		}
		time.Sleep(time.Millisecond)
	}
}

// A check that ignores its context must not hold the runner open. The
// real one shells out to python on the very driver this package exists
// to survive, and a process stuck in D state does not return.
func TestACheckThatIgnoresItsDeadlineDoesNotStallTheRunner(t *testing.T) {
	now, _ := clock(t)
	blocked := make(chan struct{})
	defer close(blocked)
	r := New(Config{
		Cards:   func(context.Context) []Card { return cards("a") },
		Try:     func(context.Context, Card) error { <-blocked; return nil },
		Timeout: 20 * time.Millisecond,
		Now:     now,
	})
	done := make(chan struct{})
	go func() { r.RunOnce(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the round never ended, so no later round can run either")
	}
	outcome, ok := r.Outcomes()["a"]
	if !ok || outcome.OK || !strings.Contains(outcome.Detail, "did not finish") {
		t.Fatalf("outcome = %+v, want it recorded as not finishing in time", outcome)
	}
	// And the runner is free for the next round.
	r.RunOnce(context.Background())
}

// A check abandoned at its deadline is still waiting on the driver. The
// next round must not start another beside it: a card stuck in D state
// would gain one python process every quarter hour for as long as it
// stays stuck. Once the first one returns, the card is asked again.
func TestAStuckCheckIsNotJoinedByAnother(t *testing.T) {
	now, advance := clock(t)
	release := make(chan struct{})
	var mu sync.Mutex
	started := 0
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(context.Context, Card) error {
			mu.Lock()
			started++
			first := started == 1
			mu.Unlock()
			if first {
				<-release
			}
			return nil
		},
		Timeout: 20 * time.Millisecond,
		Now:     now,
	})
	r.RunOnce(context.Background())
	r.RunOnce(context.Background())
	mu.Lock()
	if started != 1 {
		t.Fatalf("checks started = %d while the first is still stuck, want 1", started)
	}
	mu.Unlock()
	if outcome := r.Outcomes()["a"]; outcome.OK || !strings.Contains(outcome.Detail, "has not returned") {
		t.Fatalf("outcome = %+v, want the card failed for the check it is stuck in", outcome)
	}

	close(release)
	// The abandoned check counted as a failure, so the card also has to reach
	// the end of that failure's backoff before it can be asked again.
	advance(16 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.RunOnce(context.Background())
		if outcome := r.Outcomes()["a"]; outcome.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the card was never asked again after its stuck check returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
