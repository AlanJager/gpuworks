package cardsmoke

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// While a card is being asked, and for a grace period after, it reads as
// touched; WaitIdle waits for the attempt and no longer.
func TestAttemptsAreSeenWhileInFlightAndJustAfter(t *testing.T) {
	now, advance := clock(t)
	release := make(chan struct{})
	started := make(chan struct{})
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, c Card) error {
			close(started)
			<-release
			return nil
		},
		Interval: time.Hour, Timeout: 10 * time.Second, Now: now,
	})
	if r.Touched("a", 15*time.Second) {
		t.Fatal("touched before any attempt")
	}
	done := make(chan struct{})
	go func() { r.RunOnce(context.Background()); close(done) }()
	<-started
	if !r.Touched("a", 0) || r.Touched("b", time.Hour) {
		t.Fatal("an attempt in flight is not seen on its card alone")
	}
	if r.WaitIdle(t.Context(), []string{"a"}, 100*time.Millisecond) {
		t.Fatal("WaitIdle returned free while the attempt was running")
	}
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	if !r.WaitIdle(t.Context(), []string{"x", "a"}, 5*time.Second) {
		t.Fatal("WaitIdle did not see the attempt end")
	}
	<-done
	if !r.Touched("a", 15*time.Second) {
		t.Fatal("a card asked a moment ago is not touched")
	}
	advance(16 * time.Second)
	if r.Touched("a", 15*time.Second) {
		t.Fatal("still touched past the grace period")
	}
	var nilRunner *Runner
	if nilRunner.Touched("a", time.Hour) || !nilRunner.WaitIdle(t.Context(), []string{"a"}, time.Second) {
		t.Fatal("a node without a runner must read as untouched and idle")
	}
}

// A card that an instance or a foreign process took while it was being asked
// is not blamed for the failure, and earns no backoff; a card that failed on
// its own still is.
func TestAFailureOnACardClaimedMeanwhileIsNotTheCards(t *testing.T) {
	now, advance := clock(t)
	var claimed, foreign atomic.Bool
	tries := 0
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(context.Context, Card) error {
			tries++
			if tries == 1 {
				claimed.Store(true) // the engine opened the die first
			}
			if tries == 2 {
				foreign.Store(true)
			}
			return errors.New("aclrtSetDevice=507033")
		},
		Busy:     func(Card) bool { return claimed.Load() },
		Foreign:  func(Card) bool { return foreign.Load() },
		Interval: time.Hour, Now: now,
	})
	r.RunOnce(context.Background())
	if _, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("outcome recorded for a card an instance took mid-check: %+v", r.Outcomes()["a"])
	}

	// CheckNow asks a held card on purpose; only a foreign claim excuses it.
	r.CheckNow(context.Background(), map[string]bool{"a": true})
	if _, ok := r.Outcomes()["a"]; ok {
		t.Fatalf("outcome recorded for a card taken outside the platform mid-check")
	}

	claimed.Store(false)
	foreign.Store(false)
	advance(2 * time.Hour)
	r.RunOnce(context.Background())
	if got := r.Outcomes()["a"]; got.OK || got.Failures != 1 {
		t.Fatalf("outcome = %+v, want the card's own failure recorded", got)
	}
}
