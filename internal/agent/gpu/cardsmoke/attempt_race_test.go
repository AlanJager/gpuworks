package cardsmoke

import (
	"context"
	"testing"
	"time"
)

// A check that returns the instant its deadline passes races the runner
// marking it stuck: it can come back, and find no mark to clear, before the
// mark is set. Its answer is still waiting to be read, so the mark must not
// outlive it, or every later check on the node is refused as if a process
// were still in the driver. The caller's deadline is less than recheckSlack
// away, so the runner gives a late answer no time at all (settleWithin is 0),
// which is when the race decides the outcome. It is repeated because it is a
// race: under -race the old code left the mark within a few hundred tries.
func TestAnAttemptThatReturnsAtItsDeadlineLeavesNoStuckMark(t *testing.T) {
	r := New(Config{
		Cards: func(context.Context) []Card { return cards("a") },
		Try: func(ctx context.Context, _ Card) error {
			deadline, _ := ctx.Deadline()
			time.Sleep(time.Until(deadline))
			return nil
		},
		Timeout:   time.Millisecond,
		ReapGrace: 100 * time.Millisecond,
	})
	card := cards("a")[0]
	for i := range 1000 {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		res := r.attempt(ctx, card, time.Millisecond)
		if res.back != nil {
			<-res.back
		}
		cancel()
		r.mu.Lock()
		stuck := r.stuck["a"]
		delete(r.rechecking, "a")
		r.mu.Unlock()
		if stuck != 0 {
			t.Fatalf("attempt %d: the card is still marked stuck (%s) after its check returned", i, stuck)
		}
	}
}
