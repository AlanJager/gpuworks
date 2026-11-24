//go:build linux && dqlite

package app

import (
	"fmt"
	"log/slog"
	"math/bits"
	"runtime"
	"strings"
	"sync"
	"time"
)

// admissionJournal leaves, at the default log level, what a reader needs to
// say why a member's business closed and why it stayed closed: who closed
// admission, and what each proof failed on while it was closed. It decides
// nothing. Lines come at the first, second, fourth, eighth occurrence and so
// on within one closed period, and one line ends the period.
type admissionJournal struct {
	mu       sync.Mutex
	closedAt time.Time
	closes   uint64
	failed   uint64
}

// closed notes one close of admission by caller.
func (j *admissionJournal) closed(log *slog.Logger, caller string) {
	j.mu.Lock()
	if j.closedAt.IsZero() {
		j.closedAt = time.Now()
	}
	j.closes++
	n := j.closes
	j.mu.Unlock()
	if log != nil && bits.OnesCount64(n) == 1 {
		log.Info("closed business admission", "caller", caller, "closes", n)
	}
}

// proof notes one finished proof. succeeded is a proof that can reopen the
// member: ready, without error, and returned inside its window.
func (j *admissionJournal) proof(log *slog.Logger, succeeded bool, errClass, stage, class string, took time.Duration) {
	j.mu.Lock()
	wasClosed, closedAt := !j.closedAt.IsZero(), j.closedAt
	if succeeded {
		failed, closes := j.failed, j.closes
		j.closedAt, j.closes, j.failed = time.Time{}, 0, 0
		j.mu.Unlock()
		if log != nil && wasClosed {
			log.Info("candidate material proof succeeded after business admission closed", "failed_proofs", failed, "closes", closes,
				"closed_ms", float64(time.Since(closedAt))/float64(time.Millisecond))
		}
		return
	}
	if !wasClosed {
		// The member still serves on its last confirmation; the debug line
		// has this attempt.
		j.mu.Unlock()
		return
	}
	j.failed++
	n := j.failed
	j.mu.Unlock()
	if log != nil && bits.OnesCount64(n) == 1 {
		log.Info("candidate material proof failed while business admission is closed", "failed_proofs", n,
			"err_class", errClass, "failure_stage", stage, "failure_class", class,
			"total_ms", float64(took)/float64(time.Millisecond),
			"closed_ms", float64(time.Since(closedAt))/float64(time.Millisecond))
	}
}

// admissionCloser names the function that asked for admission to be closed:
// the first frame above the closing helpers themselves.
func admissionCloser() string {
	var pcs [12]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		name := frame.Function
		if name != "" && !strings.HasSuffix(name, ").closeBusinessAdmission") &&
			!strings.HasSuffix(name, ").invalidateCandidateMaterialEligibility") &&
			!strings.HasSuffix(name, "materialEligibility).Invalidate") {
			if cut := strings.LastIndex(name, "/"); cut >= 0 {
				name = name[cut+1:]
			}
			return fmt.Sprintf("%s:%d", name, frame.Line)
		}
		if !more {
			return "unknown"
		}
	}
}
