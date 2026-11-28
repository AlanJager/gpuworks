package cardsmoke

import (
	"context"
	"time"
)

// Checks made outside the runner's rounds, and what the rounds left unasked.

// Why a card was left unasked.
const (
	// SkipBusy: an instance serves on the card.
	SkipBusy = "busy"
	// SkipForeign: somebody outside the platform uses the card.
	SkipForeign = "foreign"
	// SkipDeferred: what the node runs is not known yet.
	SkipDeferred = "deferred"
	// SkipNotHeld: the platform does not hold the card (on a Kubernetes node,
	// it is the kubelet's to give to any pod), so it is not asked.
	SkipNotHeld = "not_held"
	// SkipUnavailable: the host cannot be asked right now.
	SkipUnavailable = "unavailable"
	// SkipInProgress: another runtime check was in progress, so this one
	// asked nothing. Either it was this card's own (a timed-out attempt
	// waiting to come back or being checked again, which answers for the
	// card), or one on another card that held the runtime for all the time
	// this one had (Queued).
	SkipInProgress = "in_progress"
)

// Record takes an answer the card gave to a check made outside the runner
// (Doctor's) as if a round had asked it: a pass clears the card's failure and
// its backoff straight away, a failure counts and backs off like one of the
// runner's own. Without it a card that failed would stay unusable until its
// backoff — up to MaxBackoffRounds intervals — let a round ask it again,
// however clearly a diagnosis had just shown it working.
//
// Only an answer about the card belongs here. A check that could not reach
// the runtime, or was cut off by the caller's own budget, has no verdict and
// is not recorded; nor has one that ran out of its timeout without the wider
// re-check a round makes before it judges a timeout (attemptWithRecheck).
func (r *Runner) Record(ident string, err error) {
	r.RecordTimed(ident, err, 0)
}

// RecordTimed is Record with how long the check took.
func (r *Runner) RecordTimed(ident string, err error, took time.Duration) {
	if r == nil || ident == "" {
		return
	}
	at := r.cfg.Now()
	r.mu.Lock()
	failures := r.outcomes[ident].Failures
	r.mu.Unlock()
	outcome := Outcome{OK: err == nil, At: at, Duration: took}
	if err != nil {
		outcome.Detail = err.Error()
		outcome.Failures = failures + 1
	}
	r.record(ident, outcome)
	r.scheduleNext(ident, outcome, at)
}

// CheckCardBudget is the longest CheckCard spends asking a card: the first
// attempt, the wait for it to come back once killed, the wider re-check and
// the moment the re-check leaves before the caller's deadline. A caller that
// gives CheckCard less than this (and the time it takes to list the cards)
// gets the first timeout as the answer, not the re-check's.
func (r *Runner) CheckCardBudget() time.Duration {
	if r == nil {
		return 0
	}
	if r.cfg.WideTimeout <= r.cfg.Timeout {
		return r.cfg.Timeout
	}
	return r.cfg.Timeout + r.cfg.ReapGrace + r.cfg.WideTimeout + recheckSlack
}

// CheckCard asks about one card now, whatever its schedule or backoff, and
// returns what it answered, or why it was not asked (a Skip* reason). A card
// an instance serves on, or somebody outside the platform uses, is left alone
// like a round leaves it; so is every card while the host cannot be asked. A
// card whose timed-out check is being checked again, or that could not be
// asked because another check held the runtime all along, is SkipInProgress.
func (r *Runner) CheckCard(ctx context.Context, ident string) (Outcome, string) {
	if r == nil {
		return Outcome{}, SkipUnavailable
	}
	if reason := r.unavailable(); reason != "" {
		return Outcome{}, SkipUnavailable
	}
	if r.deferredReason() != "" {
		return Outcome{}, SkipDeferred
	}
	for _, card := range r.cfg.Cards(ctx) {
		if card.Ident != ident {
			continue
		}
		switch {
		case r.cfg.Foreign != nil && r.cfg.Foreign(card):
			return Outcome{}, SkipForeign
		case r.busy(card):
			return Outcome{}, SkipBusy
		}
		before, heldBefore := r.answerCount(ident), r.notHeldCount(ident)
		_, inProgress := r.tryUnlessBusy(ctx, card, false)
		if r.notHeldCount(ident) != heldBefore {
			return Outcome{}, SkipNotHeld
		}
		if inProgress != "" {
			// The card's own timed-out check is being waited for or
			// checked again, or another check held the runtime all along:
			// that check answers, at the next report.
			return Outcome{}, inProgress
		}
		after, ok := r.Outcomes()[ident]
		if !ok || r.answerCount(ident) == before {
			// Inconclusive, cancelled, stuck or claimed while asked:
			// nothing was decided about the card.
			return Outcome{}, SkipUnavailable
		}
		return after, ""
	}
	return Outcome{}, SkipUnavailable
}

// Skips is why each card was last left unasked, for the cards whose last
// skip is newer than their last answer.
func (r *Runner) Skips() map[string]Skip {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Skip, len(r.skips))
	for ident, skip := range r.skips {
		out[ident] = skip
	}
	return out
}

func (r *Runner) skip(ident, reason string) {
	r.mu.Lock()
	r.skips[ident] = Skip{Reason: reason, At: r.cfg.Now()}
	r.mu.Unlock()
}

func (r *Runner) skipAll(cards []Card, reason string) {
	for _, card := range cards {
		r.skip(card.Ident, reason)
	}
}

func (r *Runner) notHeldCount(ident string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notHeld[ident]
}

func (r *Runner) answerCount(ident string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answers[ident]
}
