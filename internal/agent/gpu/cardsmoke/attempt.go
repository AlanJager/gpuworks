package cardsmoke

import (
	"context"
	"fmt"
	"time"
)

// One attempt on one card under the runner's own deadline, and the single
// wider re-check a timed-out attempt earns.

// WideTimeoutFactor is how many times Timeout the one re-check after a
// timed-out attempt is given (Config.WideTimeout's default).
//
// A deadline is a guess at how long a working card takes, and it is wrong in
// one direction only: on an H20-3e ×8 node (driver 580.126.09, CUDA 13.0)
// healthy cards ran past the 30 s budget, were judged unusable and left
// scheduling until an operator put them back, while a card whose driver
// refuses it answers within a second with an error rather than a timeout.
// Three times the budget — 90 s on NVIDIA and Hygon, 135 s on Ascend on a
// Kubernetes node, 225 s on Ascend elsewhere and on PPU, whose check may run
// in a container — is ample for a card that is merely slow to create its first
// context, and a card that cannot finish in that either is as unusable as one
// that says so. A card that answers within the budget never waits for it. A
// slow one pays the timed-out attempt, the reap grace and the re-check once;
// after that it is given the wide budget from the start for as long as it
// keeps needing more than the ordinary one, and so is the rest of a round in
// which any card needed it (Runner.firstBudget), so neither a slow card nor a
// host whose cards are all slow pays two attempts per card every round. Only
// a card that times out on the wide budget too, which costs it the whole of
// it, is spaced out by the failure backoff.
const WideTimeoutFactor = 3

// DefaultReapGrace is how long after its deadline a killed attempt is waited
// for to come back before the runner gives up on it, marks it stuck and makes
// no re-check. The kill and the deadline are the same instant, so an attempt
// whose process dies at once still comes back a moment later: the host's check
// waits up to three seconds for the killed process to exit and a further
// second for its output (gpu.RunTracked), and returns either way. Five seconds
// covers both. A check that cleans up more after its deadline says so
// (Config.ReapGrace): one in a container stops and removes it first.
//
// Coming back does not mean the process has left the driver: RunTracked
// returns after its grace even when the process ignored the kill. What keeps a
// second process off the driver then is RunTracked's own stuck mark under
// gpu.RuntimeCheckKey: the re-check is refused with ErrStillRunning without
// starting anything, the vendor check turns that into Inconclusive, and the
// first timeout stands (attemptWithRecheck).
const DefaultReapGrace = 5 * time.Second

// Recheck says that the outcome is the answer to the wider re-check, made
// because the first attempt ran out of FirstTimeout and returned. Its zero
// value means the outcome is the first attempt's.
type Recheck struct {
	// FirstTimeout is what the first attempt was given and ran out of.
	FirstTimeout time.Duration
	// Timeout is what the re-check was given.
	Timeout time.Duration
}

// Made reports whether the outcome is a re-check's.
func (c Recheck) Made() bool { return c.Timeout > 0 }

// attemptResult is what one attempt came to.
type attemptResult struct {
	err  error
	took time.Duration
	// timedOut is that the deadline decided the answer, whatever Try said.
	timedOut bool
	// returned is that Try came back. An attempt that did not is marked
	// stuck and stays so until it does.
	returned bool
	// cancelled is that the round's own context ended: the attempt says
	// nothing about the card and is discarded.
	cancelled bool
	// back is closed once Try has come back, for an attempt the deadline
	// decided before it had; late then holds what it said.
	back <-chan struct{}
	late <-chan tryAnswer
}

// tryAnswer is what Try returned, and when.
type tryAnswer struct {
	err        error
	finishedAt time.Time
}

func timeoutError(timeout time.Duration) error {
	return fmt.Errorf("the runtime check did not finish within %s", timeout)
}

// attempt runs Try once on the card with timeout as its deadline.
func (r *Runner) attempt(ctx context.Context, card Card, timeout time.Duration) attemptResult {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The deadline is the runner's, not only the attempt's. A check that
	// ignores its context — a python stuck in D state on the very driver
	// this package exists to survive — would otherwise hold the round
	// open forever, and with it every round after it.
	done := make(chan tryAnswer, 1)
	back := make(chan struct{})
	started := time.Now()
	end := r.Begin(card.Ident)
	go func() {
		err := r.cfg.Try(bounded, card)
		end()
		defer close(back)
		// Sent before the mark is cleared, so the runner either sees the
		// answer or finds the mark already gone, never a mark left behind.
		done <- tryAnswer{err: err, finishedAt: time.Now()}
		r.mu.Lock()
		delete(r.stuck, card.Ident)
		r.mu.Unlock()
	}()
	deadline, _ := bounded.Deadline()
	expired := timeoutError(timeout)
	// When done and the deadline are both ready, select's random choice must
	// not decide the verdict. The attempt state and completion timestamp give
	// both paths the same answer; a returned attempt is never left marked stuck.
	// An attempt that spent its time waiting for another check's turn ran
	// out of time without asking the card (Queued): that is not a timeout.
	grade := func(a tryAnswer) attemptResult {
		res := attemptResult{err: a.err, took: a.finishedAt.Sub(started), returned: true}
		if !queued(a.err) && (bounded.Err() != nil || !a.finishedAt.Before(deadline)) {
			res.err, res.timedOut = expired, true
		}
		return res
	}
	var res attemptResult
	select {
	case a := <-done:
		res = grade(a)
	case <-bounded.Done():
		if ctx.Err() != nil {
			// The round was cancelled (agent shutdown), not the card's
			// deadline. Nothing is marked stuck because the runner is going
			// away.
			return attemptResult{cancelled: true}
		}
		res = attemptResult{err: expired, took: timeout, timedOut: true, back: back, late: done}
		r.mu.Lock()
		r.stuck[card.Ident] = timeout
		// Until attemptWithRecheck has decided, under the same lock as the
		// stuck mark, so no other check of the card reads the mark alone
		// as a failure.
		r.rechecking[card.Ident] = true
		r.mu.Unlock()
		// The check may have returned between the deadline and this mark,
		// or be returning: one that only waited for another check's turn
		// gives up the moment its deadline passes, and must be read as
		// that rather than as the card's timeout.
		settle := time.NewTimer(settleWithin(ctx, min(queuedSettle, r.cfg.ReapGrace)))
		var a tryAnswer
		came := false
		select {
		case a = <-done:
			came = true
		case <-settle.C:
		case <-ctx.Done():
		}
		settle.Stop()
		if !came {
			// A check that returned before the mark was set found no mark to
			// clear, and its answer waits in done. When done and the timer
			// (or the caller's end) are ready together, select may pick
			// either, so done is read once more before the mark is left.
			select {
			case a = <-done:
				came = true
			default:
			}
		}
		if came {
			res = grade(a)
			r.mu.Lock()
			delete(r.stuck, card.Ident)
			r.mu.Unlock()
		}
	}
	if res.err != nil && ctx.Err() != nil {
		// A failure that arrived with the round's cancellation is the
		// shutdown's, not the card's.
		res.cancelled = true
	}
	return res
}

// attemptWithRecheck is attempt, then — only when the first ran out of its
// deadline and came back within ReapGrace of it — once more with WideTimeout.
// The re-check's answer replaces the first one; a re-check that could not
// reach the card leaves the first answer standing, so the card is judged as it
// would have been without it.
//
// Time spent waiting for another check's turn is not the card's (Queued): a
// first attempt or a re-check that only waited is no answer, and nothing is
// recorded about the card.
//
// No re-check starts while any check on this node is stuck: the process
// behind it is still waiting on the driver, and a second one would join the
// same wait (the pile-up tryUnlessBusy guards against). A card taken by an
// instance or by somebody outside the platform during the first attempt is not
// re-asked either: its timeout is theirs, and nothing is recorded about it.
//
// The re-check is not waited for or started when the caller's own deadline
// would end it first (an on-demand check whose caller gave it less than
// Runner.CheckCardBudget): it would be cut short as a cancellation and decide
// nothing, where the first timeout is an answer. Its detail says the card was
// not checked again, and why.
//
// A context cancelled while the killed attempt is waited for is the round's
// end, as it is during an attempt: the result is cancelled, and nothing is
// recorded about the card.
//
// From the first deadline until this returns the card is rechecking: another
// check asked about it meanwhile (CheckNow, CheckCard) is in progress and
// records nothing, rather than reading the stuck mark of the killed attempt
// as a failure.
func (r *Runner) attemptWithRecheck(ctx context.Context, card Card, skipBusy bool) (attemptResult, Recheck) {
	defer func() {
		r.mu.Lock()
		delete(r.rechecking, card.Ident)
		r.mu.Unlock()
	}()
	budget := r.firstBudget(card.Ident)
	first := r.attempt(ctx, card, budget)
	if first.cancelled || !first.timedOut || r.cfg.WideTimeout <= budget {
		return first, Recheck{}
	}
	r.mu.Lock()
	r.rechecking[card.Ident] = true
	r.mu.Unlock()
	if !first.returned {
		if !fitsBefore(ctx, r.cfg.ReapGrace+r.cfg.WideTimeout) {
			return r.notRechecked(ctx, first, budget), Recheck{}
		}
		back := r.cameBack(ctx, first.back)
		if ctx.Err() != nil {
			first.cancelled = true
			return first, Recheck{}
		}
		if !back {
			// Still in the driver: it stays marked stuck, and the card is
			// judged on its first timeout as it always was.
			return first, Recheck{}
		}
		select {
		case a := <-first.late:
			if queued(a.err) {
				// It came back the moment its deadline passed, having
				// waited for another check's turn all along.
				return attemptResult{err: a.err, took: first.took, returned: true}, Recheck{}
			}
		default:
		}
	}
	if r.anyStuck() || r.claimedDuring(card, skipBusy) {
		return first, Recheck{}
	}
	if !fitsBefore(ctx, r.cfg.WideTimeout) {
		return r.notRechecked(ctx, first, budget), Recheck{}
	}
	wide := r.attempt(ctx, card, r.cfg.WideTimeout)
	if wide.cancelled || queued(wide.err) {
		// Cancelled, or it waited out the wide budget for another check's
		// turn: neither the re-check nor the first timeout decides.
		return wide, Recheck{}
	}
	if inconclusiveReason(wide.err) != "" {
		// The runtime could not be asked again (an earlier check's process
		// is still on the driver): the first answer stands.
		return first, Recheck{}
	}
	recheck := Recheck{FirstTimeout: budget, Timeout: r.cfg.WideTimeout}
	if wide.timedOut {
		wide.err = fmt.Errorf("the runtime check did not finish within %s, nor within %s when checked again", budget, r.cfg.WideTimeout)
	} else if wide.err == nil {
		r.mu.Lock()
		if r.running {
			r.wideRound = true
		}
		r.mu.Unlock()
	}
	return wide, recheck
}

// firstBudget is what an attempt on the card is given first: WideTimeout for
// a card whose last answer was a pass that needed more than Timeout (on the
// wider re-check, or on a wide first attempt that took longer than Timeout),
// and for the rest of a round in which a card has already needed it; Timeout
// otherwise. A card that is slow but healthy then pays one wide attempt per
// round rather than a timed-out one, the reap grace and a wide one, and a
// host whose cards are all slow does not: eight H20-3e cards at 30+5+90 s
// each would take longer than the interval between rounds. A card that
// answers within Timeout again, or fails, starts from Timeout the next round.
func (r *Runner) firstBudget(ident string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wideRound || r.neededWide(r.outcomes[ident]) {
		return r.cfg.WideTimeout
	}
	return r.cfg.Timeout
}

// neededWide reports whether the outcome is a pass that took more than
// Timeout. Caller holds r.mu.
func (r *Runner) neededWide(o Outcome) bool {
	return o.OK && (o.Recheck.Made() || o.Duration >= r.cfg.Timeout)
}

// notRechecked is the first timeout as the answer when the caller's deadline
// leaves no room for the re-check, saying so: the card was not asked again,
// and the caller is why.
func (r *Runner) notRechecked(ctx context.Context, first attemptResult, budget time.Duration) attemptResult {
	left := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		left = max(0, time.Until(deadline))
	}
	if left >= time.Second {
		left = left.Round(time.Second)
	} else {
		left = left.Round(time.Millisecond)
	}
	first.err = fmt.Errorf("the runtime check did not finish within %s; it was not checked again with %s, because its caller had only %s left", budget, r.cfg.WideTimeout, left)
	return first
}

// queuedSettle is how long after its deadline an attempt is waited for before
// it is taken for the card's timeout. An attempt that only waited for another
// check's turn returns at its deadline, having started nothing (Queued); this
// lets it say so. It is part of ReapGrace, not added to it, and well inside
// recheckSlack.
const queuedSettle = 250 * time.Millisecond

// settleWithin is d, or less so that it ends recheckSlack before ctx's
// deadline: waiting into the caller's deadline would turn the card's timeout
// into a cancellation.
func settleWithin(ctx context.Context, d time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		d = min(d, time.Until(deadline)-recheckSlack)
	}
	return max(0, d)
}

// recheckSlack is how long before the caller's deadline a re-check must end,
// so that its own deadline, not the caller's, decides it.
const recheckSlack = time.Second

// fitsBefore reports whether d, and recheckSlack after it, ends before ctx's
// deadline.
func fitsBefore(ctx context.Context, d time.Duration) bool {
	deadline, bounded := ctx.Deadline()
	return !bounded || time.Until(deadline) >= d+recheckSlack
}

// cameBack waits up to ReapGrace for a killed attempt to return. Meanwhile the
// card is marked stuck, so nothing else starts on the node, and rechecking, so
// another check asked about the same card is told one is in progress rather
// than recorded as a failure (tryUnlessBusy).
func (r *Runner) cameBack(ctx context.Context, back <-chan struct{}) bool {
	if back == nil {
		return false
	}
	grace := time.NewTimer(r.cfg.ReapGrace)
	defer grace.Stop()
	select {
	case <-back:
		return true
	case <-grace.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) anyStuck() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stuck) > 0
}
