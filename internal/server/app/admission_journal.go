//go:build linux && dqlite

package app

import (
	"log/slog"
	"math/bits"
	"sync"
	"time"
)

// admissionJournal leaves, at the default log level, what a reader needs to
// say why a member's business closed and why it stayed closed: the cause of
// the close, and what each proof ended on while it was closed. It decides
// nothing.
//
// A closed period begins with the first close and ends when the proof worker
// accepts a proof. In it: one line for the close that began it, one for each
// further cause of a close, a line for failed proofs at the first,
// second, fourth, eighth and so on, and one line that ends it.
type admissionJournal struct {
	mu       sync.Mutex
	closedAt time.Time
	callers  map[admissionCloseCause]bool
	failed   uint64
	// attempt is what the proof in progress ended on, kept until the worker
	// says what it made of it.
	attempt admissionAttempt
}

type admissionAttempt struct {
	ready                  bool
	errClass, stage, class string
	took                   time.Duration
}

// closed notes a close of admission and its cause. A cause outside the list
// is logged as the list's catch-all, so the journal never carries a value
// nobody documented. While the member is already closed, the proof worker's
// own closes are the failed proofs, which have their own line.
func (j *admissionJournal) closed(log *slog.Logger, cause admissionCloseCause) {
	byProof, listed := admissionCloseCauses[cause]
	if !listed {
		cause = closeOther
	}
	j.mu.Lock()
	first := j.closedAt.IsZero()
	if first {
		j.closedAt, j.callers = time.Now(), map[admissionCloseCause]bool{}
	}
	seen := j.callers[cause]
	if !seen && (first || !byProof) {
		j.callers[cause] = true
	}
	j.mu.Unlock()
	if log == nil || seen || (!first && byProof) {
		return
	}
	log.Info("closed business admission", "caller", string(cause), "first_of_period", first)
}

// attempted keeps what the proof in progress ended on.
func (j *admissionJournal) attempted(ready bool, errClass, stage, class string, took time.Duration) {
	j.mu.Lock()
	j.attempt = admissionAttempt{ready: ready, errClass: errClass, stage: stage, class: class, took: took}
	j.mu.Unlock()
}

// outcome is told what the proof worker made of the last attempt, after any
// close that attempt caused. proved ends a closed period; anything else is a
// failed proof of a closed member, the one whose answer closed it included.
func (j *admissionJournal) outcome(log *slog.Logger, proved bool) {
	j.mu.Lock()
	attempt, closedAt := j.attempt, j.closedAt
	j.attempt = admissionAttempt{}
	if closedAt.IsZero() {
		// The member serves: on this proof, or still on its last one.
		j.mu.Unlock()
		return
	}
	if proved {
		failed, callers := j.failed, len(j.callers)
		j.closedAt, j.callers, j.failed = time.Time{}, nil, 0
		j.mu.Unlock()
		if log != nil {
			log.Info("candidate material proof succeeded after business admission closed", "failed_proofs", failed, "close_callers", callers,
				"closed_ms", float64(time.Since(closedAt))/float64(time.Millisecond))
		}
		return
	}
	j.failed++
	n := j.failed
	j.mu.Unlock()
	if log == nil || bits.OnesCount64(n) != 1 {
		return
	}
	stage := attempt.stage
	if attempt.ready {
		// The attempt itself succeeded; the worker did not accept it: it
		// returned after its window, or admission changed under it.
		stage = "result_not_accepted"
	}
	log.Info("candidate material proof failed while business admission is closed", "failed_proofs", n,
		"err_class", attempt.errClass, "failure_stage", stage, "failure_class", attempt.class,
		"total_ms", float64(attempt.took)/float64(time.Millisecond),
		"closed_ms", float64(time.Since(closedAt))/float64(time.Millisecond))
}
