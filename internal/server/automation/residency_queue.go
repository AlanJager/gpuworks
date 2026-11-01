package automation

import (
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// waiting is one deployment in the arbiter's queue: requests wait for it
// (or a replica of it waits to be placed), or it only waits to take back the
// cards it gave up (resume).
type waiting struct {
	id     uint
	policy yieldpolicy.Policy
	since  time.Time
	resume bool
	place  bool
}

// queueBefore orders the arbiter's queue. The queue is served in order and a
// holder yields once per look, so the first waiter a holder blocks gets its
// cards. A higher rank goes first. Within a rank a deployment something is
// waiting for - a request, or a replica to place - goes before one that only
// waits to take its cards back: a resume answers no one, and serving it first
// makes the waiting request wait for the resumer's idle time or turn on top
// (docs/explanation/service-levels.md). Then the one waiting longest goes
// first, so neither kind starves its own.
func queueBefore(a, b waiting) bool {
	if ra, rb := a.policy.Rank(), b.policy.Rank(); ra != rb {
		return ra > rb
	}
	if a.resume != b.resume {
		return !a.resume
	}
	return a.since.Before(b.since)
}

func queueText(queue []waiting) []string {
	out := make([]string, len(queue))
	for i, w := range queue {
		kind := "request"
		switch {
		case w.place:
			kind = "place"
		case w.resume:
			kind = "resume"
		}
		out[i] = fmt.Sprintf("%d:%s:%s", w.id, w.policy.Yield, kind)
	}
	return out
}
