//go:build linux && dqlite

package app

import (
	"log/slog"
	"math/bits"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var businessRefusalCodeSet = func() map[string]bool {
	set := make(map[string]bool, len(businessRefusalCodes))
	for _, code := range businessRefusalCodes {
		set[code] = true
	}
	return set
}()

// gateReasons translates the reasons the gate and the material proof give in
// their own words (the readiness log line keeps those) to refusal codes.
var gateReasons = map[string]string{
	"no_business_generation": refusalNoGeneration,
	"generation_changed":     refusalGenerationChanged,
	"proof_missing":          refusalProofMissing,
	"expired":                refusalProofExpired,
	"stopped":                refusalProofStopped,
	"run_canceled":           refusalProofRunCanceled,
	"proof_canceled":         refusalProofCanceled,
	"headroom":               refusalProofHeadroom,
	"epoch_changed":          refusalProofEpochChanged,
	"proof_owner_changed":    refusalProofOwnerChanged,
	"renewal_unavailable":    refusalRenewalUnavailable,
	"request_canceled":       refusalRequestCanceled,
	"request_deadline":       refusalRequestDeadline,
}

func gateRefusalCode(reason string) string {
	if code, ok := gateReasons[reason]; ok {
		return code
	}
	if businessRefusalCodeSet[reason] {
		return reason
	}
	return refusalOther
}

// businessRecoveryLogEvery is the least time between two lines that say
// business is served again: a member that alternates between refusing and
// serving leaves one line per interval, with everything refused since the
// line before.
const businessRecoveryLogEvery = 10 * time.Second

// businessRefusals counts the business requests this process refused, by
// reason, and reports them. A run is the refusals between two such lines.
// Within a run each reason is logged at its first, second, fourth, eighth
// refusal and so on, so every incident starts with a line whatever came
// before it. A request that is served costs one atomic read.
type businessRefusals struct {
	log atomic.Pointer[slog.Logger]
	// open is set by a refusal and cleared by the next served request.
	open atomic.Bool
	// recoveryEvery replaces businessRecoveryLogEvery in tests.
	recoveryEvery time.Duration

	mu       sync.Mutex
	total    map[string]uint64 // since the process started
	run      map[string]uint64 // in the current run
	runStart time.Time
	runCount uint64
	reported time.Time // the last recovery line
	pending  bool      // a recovery line is scheduled
}

// newBusinessRefusals is a member's record. A member keeps one for as long
// as its process runs, across the generations of its business, and hands it
// to whatever answers business requests: the gate, the member handler and
// the forwarder. A nil record names reasons and counts nothing.
func newBusinessRefusals() *businessRefusals { return &businessRefusals{} }

func (b *businessRefusals) logger() *slog.Logger {
	if log := b.log.Load(); log != nil {
		return log
	}
	return slog.Default()
}

func (b *businessRefusals) every() time.Duration {
	if b.recoveryEvery != 0 {
		return b.recoveryEvery
	}
	return businessRecoveryLogEvery
}

// record counts one refusal.
func (b *businessRefusals) record(reason string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.total == nil {
		b.total, b.run = map[string]uint64{}, map[string]uint64{}
	}
	b.total[reason]++
	b.run[reason]++
	inRun, total := b.run[reason], b.total[reason]
	if b.runCount == 0 {
		b.runStart = time.Now()
	}
	b.runCount++
	b.mu.Unlock()
	b.open.Store(true)
	if bits.OnesCount64(inRun) == 1 {
		b.logger().Warn("refused a business request", "reason", reason, "refused", inRun, "since_start", total)
	}
}

// served notes that business is served again after a refusal.
func (b *businessRefusals) served() {
	if b == nil || !b.open.Load() || !b.open.CompareAndSwap(true, false) {
		return
	}
	b.mu.Lock()
	if wait := b.every() - time.Since(b.reported); !b.reported.IsZero() && wait > 0 {
		// Too soon after the last line: say it once the interval is over,
		// unless business is being refused again by then.
		if !b.pending {
			b.pending = true
			time.AfterFunc(wait, b.reportLater)
		}
		b.mu.Unlock()
		return
	}
	b.reportLocked()
}

func (b *businessRefusals) reportLater() {
	b.mu.Lock()
	b.pending = false
	if b.open.Load() {
		// Refusing again: the next served request reports the whole run.
		b.mu.Unlock()
		return
	}
	b.reportLocked()
}

// reportLocked ends the run and logs it. It unlocks b.mu.
func (b *businessRefusals) reportLocked() {
	refused, lasted := b.runCount, time.Since(b.runStart)
	b.runCount = 0
	clear(b.run)
	if refused != 0 {
		b.reported = time.Now()
	}
	b.mu.Unlock()
	if refused == 0 {
		return
	}
	b.logger().Info("business served again after refusals", "refused", refused,
		"lasted_ms", float64(lasted)/float64(time.Millisecond))
}

// snapshot is the count per reason since the process started.
func (b *businessRefusals) snapshot() map[string]uint64 {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]uint64, len(b.total))
	for reason, n := range b.total {
		out[reason] = n
	}
	return out
}

// serving marks every request that reaches next as served.
func (b *businessRefusals) serving(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !memberProbe(r) {
			b.served()
		}
		next.ServeHTTP(w, r)
	})
}

// mark names the reason on the response and counts it. A probe asks about
// the member and is not business: it gets the header and no count.
func (b *businessRefusals) mark(w http.ResponseWriter, r *http.Request, reason string) {
	if !businessRefusalCodeSet[reason] {
		reason = refusalOther
	}
	w.Header().Set(businessRefusalHeader, reason)
	if r != nil && memberProbe(r) {
		return
	}
	b.record(reason)
}

// refuse answers as a closed member does and says why.
func (b *businessRefusals) refuse(w http.ResponseWriter, r *http.Request, reason string) {
	b.mark(w, r, reason)
	answerNotReady(w)
}

// answerNotReady is a closed member's answer, without a reason or a count.
func answerNotReady(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
}
