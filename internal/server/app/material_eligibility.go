//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	// A proof may spend one membership grant timeout and still leave that
	// same timeout of freshness. The deadline and the proof context both
	// start before the first read. A proof that exceeds the window is
	// discarded instead of stretching the age of bytes it already read.
	materialEligibilityWindow = 10*time.Second + store.ClusterMembershipGrantTimeout
	// materialReproofInterval paces a proof that did not succeed, and bounds
	// how long warm verification and readiness wait behind a renewal.
	materialReproofInterval = 2 * time.Second
	// materialIdleReproofRest is how long a member rests after a successful
	// proof before it starts the next one. Each proof costs replicated grant
	// commits on this member and on the peers it asks, so the rest sets the
	// idle write rate of the whole cluster. successRest never lets it reach
	// into the last two grant timeouts of the proof just published, so the
	// renewal starts with at least 2*ClusterMembershipGrantTimeout left: one
	// proof as slow as a whole grant timeout, or one failed attempt retried
	// after materialReproofInterval and followed by a proof of up to three
	// seconds, still leaves the grant timeout of lifetime that readiness
	// requires. Two failed attempts in a row may drop below it. A value above
	// materialEligibilityWindow - 2*ClusterMembershipGrantTimeout changes
	// nothing; four seconds keeps one more second while proofs are fast.
	materialIdleReproofRest = 4 * time.Second
)

// materialProofPhase spreads member starts within the existing cadence. Mix
// the full ID so nearby IDs and IDs with the same parity do not share a bucket.
// Collisions affect scheduling only; every member still needs its own proof.
func materialProofPhase(id uint64) time.Duration {
	if id == 0 {
		return 0
	}
	id = (id ^ (id >> 30)) * 0xbf58476d1ce4e5b9
	id = (id ^ (id >> 27)) * 0x94d049bb133111eb
	id ^= id >> 31
	return time.Duration(id % uint64(materialReproofInterval))
}

// materialEligibility holds a short-lived, in-memory result of the full
// candidate material proof. The deadline starts before the proof reads its
// first byte, so a slow proof cannot extend the age of its earliest read.
// An ordinary request reads Eligible only; it never scans material files.
type materialEligibility struct {
	mu            sync.Mutex
	proofSlot     chan struct{}
	now           func() time.Time
	prove         func(context.Context) (bool, error)
	interval      time.Duration
	idleRest      time.Duration
	deadline      time.Time
	bridging      bool
	provedAt      time.Time
	epoch         uint64
	cancel        context.CancelFunc
	proofCtx      context.Context
	runCtx        context.Context
	running       bool
	stopped       bool
	onInvalidated func()
	proving       atomic.Bool
	phase         time.Duration
}

func newMaterialEligibility(now func() time.Time, prove func(context.Context) (bool, error), interval time.Duration) *materialEligibility {
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = materialReproofInterval
	}
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	return &materialEligibility{proofSlot: slot, now: now, prove: prove, interval: interval, idleRest: interval}
}

// setIdleRest replaces the rest after a successful proof. It is a no-op after
// Run starts. The retry cadence after a failed proof stays the interval.
func (e *materialEligibility) setIdleRest(rest time.Duration) {
	if e == nil || rest <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || e.stopped {
		return
	}
	e.idleRest = rest
}

func (e *materialEligibility) setOnInvalidated(fn func()) {
	e.mu.Lock()
	e.onInvalidated = fn
	e.mu.Unlock()
}

// setProofPhase delays only the first proof. It is a no-op after Run starts,
// so the default of zero still proves immediately.
func (e *materialEligibility) setProofPhase(phase time.Duration) {
	if e == nil || phase <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || e.stopped {
		return
	}
	e.phase = phase
}

func (e *materialEligibility) provingNow() bool {
	return e != nil && e.proving.Load()
}

// Eligible is the request-path check. Expiry is latched so a wall-clock
// correction cannot resurrect a result that has already expired.
func (e *materialEligibility) Eligible() bool {
	return e.EligibleFor(0)
}

// EligibleFor additionally requires enough remaining proof lifetime for the
// caller's next bounded operation. Insufficient headroom does not expire the
// proof or change ordinary business admission.
func (e *materialEligibility) EligibleFor(minRemaining time.Duration) bool {
	ready, _, _ := e.eligibleForResult(minRemaining)
	return ready
}

// Diagnostics use the same decision and clock sample, without a second probe.
func (e *materialEligibility) eligibleForResult(minRemaining time.Duration) (bool, string, time.Duration) {
	if e == nil {
		return false, "proof_missing", 0
	}
	e.mu.Lock()
	if e.stopped || e.deadline.IsZero() || (e.runCtx != nil && e.runCtx.Err() != nil) {
		reason := "proof_missing"
		if e.stopped {
			reason = "stopped"
		} else if !e.deadline.IsZero() {
			reason = "run_canceled"
		}
		e.mu.Unlock()
		return false, reason, 0
	}
	if e.proofCtx != nil && e.proofCtx.Err() != nil {
		e.deadline = time.Time{}
		onInvalidated := e.onInvalidated
		e.mu.Unlock()
		if onInvalidated != nil {
			onInvalidated()
		}
		return false, "proof_canceled", 0
	}
	now := e.now()
	remaining := e.deadline.Sub(now)
	if !e.deadline.IsZero() && !now.Before(e.deadline) {
		// A reproof that has not finished is not a negative answer. A successor
		// proof that started while the last result was still valid, or one that
		// is already proving, keeps ordinary admission until that attempt's own
		// context ends. Proofs are serialized, so a slow renewal would otherwise
		// close every member together. A grant that needs headroom still waits
		// for a published proof.
		if (e.bridging || e.proving.Load()) && e.proofCtx != nil && e.proofCtx.Err() == nil {
			e.mu.Unlock()
			if minRemaining <= 0 {
				return true, "", 0
			}
			return false, "headroom", 0
		}
		e.deadline = time.Time{}
		e.bridging = false
		onInvalidated := e.onInvalidated
		e.mu.Unlock()
		if onInvalidated != nil {
			onInvalidated()
		}
		return false, "expired", remaining
	}
	ready := minRemaining <= 0 || !now.Add(minRemaining).After(e.deadline)
	e.mu.Unlock()
	if !ready {
		return false, "headroom", remaining
	}
	return true, "", remaining
}

// waitForActiveProof yields warm verification to one captured renewal, for at
// most one existing reproof interval or the current result's remaining lifetime.
// This bounds scheduling only; the caller still obtains every fresh grant.
func (e *materialEligibility) waitForActiveProof(ctx context.Context, maxWait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return nil
	}
	e.mu.Lock()
	proofCtx := e.proofCtx
	now := e.now()
	waitFor := e.interval
	if remaining := e.deadline.Sub(now); remaining < waitFor {
		waitFor = remaining
	}
	if maxWait > 0 && maxWait < waitFor {
		waitFor = maxWait
	}
	wait := !e.stopped && !e.deadline.IsZero() &&
		(e.runCtx == nil || e.runCtx.Err() == nil) &&
		proofCtx != nil && proofCtx.Err() == nil &&
		waitFor > 0 && now.Before(e.deadline)
	e.mu.Unlock()
	if wait {
		timer := time.NewTimer(waitFor)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-proofCtx.Done():
		case <-timer.C:
		case <-e.proofSlot:
			e.proofSlot <- struct{}{}
		}
	}
	return ctx.Err()
}

// Invalidate closes eligibility immediately and cancels a proof in progress.
// A late success from that proof cannot reopen the gate.
func (e *materialEligibility) Invalidate() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.epoch++
	e.deadline = time.Time{}
	cancel := e.cancel
	onInvalidated := e.onInvalidated
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onInvalidated != nil {
		onInvalidated()
	}
}

// Reprove performs one full proof outside the request path. A proof may renew
// eligibility only if it finished within the same invalidation epoch and
// within the eligibility window measured from its start.
func (e *materialEligibility) Reprove(ctx context.Context) (bool, error) {
	if e == nil || e.prove == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		e.Invalidate()
		return false, err
	}
	select {
	case <-ctx.Done():
		e.Invalidate()
		return false, ctx.Err()
	case <-e.proofSlot:
	}
	defer func() { e.proofSlot <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		e.Invalidate()
		return false, err
	}
	e.mu.Lock()
	if e.stopped || (e.runCtx != nil && e.runCtx.Err() != nil) {
		e.mu.Unlock()
		return false, nil
	}
	proofCtx, cancel := context.WithTimeout(ctx, materialEligibilityWindow)
	e.cancel = cancel
	e.proofCtx = proofCtx
	epoch := e.epoch
	started := e.now()
	e.bridging = !e.deadline.IsZero() && started.Before(e.deadline)
	e.mu.Unlock()
	callbackDone := make(chan struct{})
	stopOnCancel := context.AfterFunc(proofCtx, func() {
		defer close(callbackDone)
		e.mu.Lock()
		if e.proofCtx != proofCtx || e.epoch != epoch {
			e.mu.Unlock()
			return
		}
		e.epoch++
		e.deadline = time.Time{}
		onInvalidated := e.onInvalidated
		e.mu.Unlock()
		if onInvalidated != nil {
			onInvalidated()
		}
	})

	// Hold this across completion so a transport refresh does not take the
	// leader writer while this proof is still publishing its result.
	e.proving.Store(true)
	defer e.proving.Store(false)
	ready, err := e.prove(proofCtx)
	e.mu.Lock()
	e.cancel = nil
	e.proofCtx = nil
	e.bridging = false
	if err == nil && proofCtx.Err() != nil {
		err = proofCtx.Err()
	}
	// A failed refresh must not throw away a result that is still inside its
	// window. Otherwise one blip closes every member together. A grant or
	// peer-idle timeout is not a negative material answer either: if the
	// earlier window has already ended, the attempt's own start still bounds
	// how long ordinary admission may continue. Cancellation, an explicit
	// invalidation, a definitive failure with no open earlier proof, and a
	// proof that outlives the window still close.
	attemptDeadline := started.Add(materialEligibilityWindow)
	live := proofCtx.Err() == nil && e.epoch == epoch && !e.stopped
	unexpired := !e.deadline.IsZero() && e.now().Before(e.deadline)
	deadlineOnly := err != nil && errors.Is(err, context.DeadlineExceeded) && live
	keepEarlier := (err != nil || !ready) && live && unexpired
	renewAttempt := !keepEarlier && deadlineOnly && e.now().Before(attemptDeadline)
	invalidated := !keepEarlier && !renewAttempt && (e.stopped || e.epoch != epoch || proofCtx.Err() != nil || err != nil || !ready || !e.now().Before(attemptDeadline))
	notifyInvalidated := invalidated && e.epoch == epoch
	if keepEarlier {
		ready = false
	} else if renewAttempt {
		// Not a successful proof: provedAt stays, so a write gate stops
		// treating the closure as transient before this deadline ends.
		e.deadline = attemptDeadline
		ready = false
	} else if invalidated {
		if e.epoch == epoch {
			e.deadline = time.Time{}
		}
		ready = false
	} else {
		e.deadline = started.Add(materialEligibilityWindow)
		e.provedAt = started
	}
	onInvalidated := e.onInvalidated
	e.mu.Unlock()
	// An already-started callback must finish before the slot can admit a
	// replacement proof; it may still be withdrawing the previous routes.
	if !stopOnCancel() {
		<-callbackDone
	}
	if notifyInvalidated && onInvalidated != nil {
		onInvalidated()
	}
	cancel()
	return ready, err
}

// successRest computes scheduling headroom only; it never publishes authority.
func (e *materialEligibility) successRest() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	remaining := e.deadline.Sub(e.now())
	// Check before subtracting: a zero deadline can saturate Sub at MinInt64.
	if remaining <= 2*store.ClusterMembershipGrantTimeout {
		return 0
	}
	return min(e.idleRest, remaining-2*store.ClusterMembershipGrantTimeout)
}

// Run proves on startup and refreshes at a bounded cadence. A definitive
// failure closes eligibility. A deadline exceeded while an earlier proof is
// still inside its window leaves that proof in place. Later successful
// proofs may restore a closed member. The caller must cancel ctx before
// releasing the Store used by prove.
func (e *materialEligibility) Run(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.running || e.stopped {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.runCtx = ctx
	e.mu.Unlock()
	defer e.Stop()
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		ready, err := e.Reprove(ctx)
		if ready && err == nil {
			// A slow success can leave an old tick pending. Rest only inside
			// the existing proof lifetime, reserving two bounded grants; this
			// does not extend eligibility or promise another proof will finish.
			quiet := e.successRest()
			if quiet > 0 {
				timer := time.NewTimer(quiet)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			ticker.Reset(e.interval)
			continue
		}
		if !e.Eligible() {
			ticker.Reset(e.interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop permanently closes this App incarnation's result and cancels its
// in-flight proof. A restarted App needs a fresh component and full proof.
func (e *materialEligibility) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.stopped = true
	e.epoch++
	e.deadline = time.Time{}
	cancel := e.cancel
	onInvalidated := e.onInvalidated
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onInvalidated != nil {
		onInvalidated()
	}
}
