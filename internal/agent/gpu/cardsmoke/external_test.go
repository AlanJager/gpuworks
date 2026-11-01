package cardsmoke

import (
	"context"
	"errors"
	"testing"
)

// A round says why it left each card alone, and an answer replaces the skip.
func TestARoundRecordsWhyItLeftEachCardAlone(t *testing.T) {
	now, advance := clock(t)
	r := &recorder{now: now}
	busy, foreign := map[string]bool{"serving": true}, map[string]bool{"theirs": true}
	runner := New(Config{
		Cards:   func(context.Context) []Card { return cards("serving", "theirs", "free") },
		Try:     r.try,
		Busy:    func(c Card) bool { return busy[c.Ident] },
		Foreign: func(c Card) bool { return foreign[c.Ident] },
		Now:     now,
	})
	runner.RunOnce(context.Background())
	skips := runner.Skips()
	if skips["serving"].Reason != SkipBusy || skips["theirs"].Reason != SkipForeign {
		t.Fatalf("skips = %+v", skips)
	}
	if _, ok := skips["free"]; ok {
		t.Fatalf("an answered card is also recorded as skipped: %+v", skips)
	}
	if outcome := runner.Outcomes()["free"]; !outcome.OK {
		t.Fatalf("free card outcome = %+v", outcome)
	}

	busy["serving"] = false
	advance(DefaultInterval)
	runner.RunOnce(context.Background())
	if _, ok := runner.Skips()["serving"]; ok {
		t.Fatal("the skip outlived the answer that followed it")
	}
}

// A round that cannot ask any card says so for every card: deferred while
// what the node runs is unknown, unavailable while the host cannot be asked.
func TestARoundThatAsksNoCardSaysWhyForEach(t *testing.T) {
	now, _ := clock(t)
	for _, tc := range []struct {
		name   string
		cfg    Config
		reason string
	}{
		{"deferred", Config{Deferred: func() string { return "not known yet" }}, SkipDeferred},
		{"unavailable", Config{Unavailable: func() string { return "driver not answering" }}, SkipUnavailable},
	} {
		r := &recorder{now: now}
		cfg := tc.cfg
		cfg.Cards = func(context.Context) []Card { return cards("a", "b") }
		cfg.Try, cfg.Now = r.try, now
		runner := New(cfg)
		runner.RunOnce(context.Background())
		skips := runner.Skips()
		if skips["a"].Reason != tc.reason || skips["b"].Reason != tc.reason || len(r.idents()) != 0 {
			t.Fatalf("%s: skips = %+v, attempts %v", tc.name, skips, r.idents())
		}
	}
}

// CheckCard asks one card now, whatever its backoff, and says why when it
// cannot: a serving card, somebody else's, a host that cannot be asked.
func TestCheckCardAsksNowOrSaysWhyNot(t *testing.T) {
	now, _ := clock(t)
	r := &recorder{now: now, fail: map[string]error{"card": errors.New("no context")}}
	busy := false
	unavailable := ""
	runner := New(Config{
		Cards:       func(context.Context) []Card { return cards("card", "other") },
		Try:         r.try,
		Busy:        func(Card) bool { return busy },
		Unavailable: func() string { return unavailable },
		Now:         now,
	})
	runner.RunOnce(context.Background())
	if !runner.backingOff("card") {
		t.Fatal("the failing card is not backing off")
	}

	r.mu.Lock()
	r.fail = nil
	r.mu.Unlock()
	outcome, skip := runner.CheckCard(context.Background(), "card")
	if skip != "" || !outcome.OK || runner.backingOff("card") {
		t.Fatalf("check = %+v %q; backing off %v", outcome, skip, runner.backingOff("card"))
	}

	busy = true
	if _, skip := runner.CheckCard(context.Background(), "card"); skip != SkipBusy {
		t.Fatalf("serving card skip = %q", skip)
	}
	busy, unavailable = false, "driver not answering"
	if _, skip := runner.CheckCard(context.Background(), "card"); skip != SkipUnavailable {
		t.Fatalf("unavailable host skip = %q", skip)
	}
	unavailable = ""
	if _, skip := runner.CheckCard(context.Background(), "missing"); skip != SkipUnavailable {
		t.Fatalf("unknown card skip = %q", skip)
	}
	var none *Runner
	if _, skip := none.CheckCard(context.Background(), "card"); skip != SkipUnavailable {
		t.Fatalf("no runner skip = %q", skip)
	}
}

// An answer carries how long the attempt took.
func TestAnAnswerCarriesItsDuration(t *testing.T) {
	now, _ := clock(t)
	r := &recorder{now: now}
	runner := New(Config{Cards: func(context.Context) []Card { return cards("card") }, Try: r.try, Now: now})
	runner.RunOnce(context.Background())
	if outcome := runner.Outcomes()["card"]; outcome.Duration <= 0 {
		t.Fatalf("outcome = %+v, want a duration", outcome)
	}
}
