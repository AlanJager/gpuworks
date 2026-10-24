// Package cardsmoke decides when each accelerator is asked whether it can
// actually run work, and remembers what it answered.
//
// Listing a card and using one are different questions. The vendor CLI can
// describe a card in full while nothing can create a context on it: on
// zstone-164 nvidia-smi answers for two cards and cuInit returns 101 for
// every one of them, because the driver keeps retrying three cards it
// cannot bring up. Only trying it says so.
//
// It is all scheduling and no vendor knowledge: what the attempt is gets
// injected, so a vendor's own runtime check plugs in without this package
// learning about it, and the tests need no accelerator.
package cardsmoke

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Defaults. One round every quarter hour costs each card a fraction of a
// second and catches a driver that broke since the last one; a card busy
// serving is skipped, so the cost does not land on a working node.
const (
	DefaultInterval = 15 * time.Minute
	DefaultTimeout  = 30 * time.Second
	// DefaultMaxBackoffRounds caps how far a card that keeps failing is
	// spaced out. A card that answers costs a fraction of a second, but one
	// that cannot be used spends the whole timeout before saying so, and on
	// a host with many of them a round would otherwise run almost back to
	// back.
	DefaultMaxBackoffRounds = 8
	// DefaultFirstRound leaves the control plane time to say what this
	// node runs, so the first round is not skipped for every card.
	DefaultFirstRound = 30 * time.Second
)

// Inconclusive marks an attempt's failure as saying nothing about the card:
// the attempt never reached it. The card is then left exactly as it was, and
// the round stops because whatever stopped the attempt would stop every other
// one in it too.
func Inconclusive(reason string) error {
	return inconclusiveError{reason: strings.TrimSpace(reason)}
}

type inconclusiveError struct{ reason string }

func (e inconclusiveError) Error() string { return e.reason }

func inconclusiveReason(err error) string {
	var inconclusive inconclusiveError
	if errors.As(err, &inconclusive) {
		if inconclusive.reason == "" {
			return "the runtime check could not be made"
		}
		return inconclusive.reason
	}
	return ""
}

// Card is what the runner needs to know about one accelerator: how to name
// it, how to ask the runtime for it alone, and how a caller refers to it.
type Card struct {
	// Ident is the card's stable identity (accelerator.CardIdent), which is
	// how its answer is remembered and matched back to it.
	Ident string
	// Visible is how the vendor check identifies this card and no other (for
	// NVIDIA, the value of CUDA_VISIBLE_DEVICES).
	Visible string
}

// Outcome is what one card last answered. Only a card that was actually
// tried has one: a card with no outcome was never asked, which is not the
// same as a card that failed and must never be read as one.
type Outcome struct {
	// OK is whether the runtime could use the card.
	OK bool
	// Detail is the failure as the attempt worded it.
	Detail string
	// At is when this was decided.
	At time.Time
	// Failures is how many times in a row this card has failed, which spaces
	// out the next attempt. It is reset by a card that answers.
	Failures int
}

// Config is what a runner needs from its surroundings. Every function is
// called from the runner's own goroutine.
type Config struct {
	// Cards lists the accelerators to try, newest view each round. The
	// round's context bounds it, so a shutdown is not waited out by a
	// driver that has stopped answering.
	Cards func(ctx context.Context) []Card
	// Try asks the runtime to use one card. A nil error means it could.
	Try func(ctx context.Context, card Card) error
	// Busy reports that the card is serving, so it is left alone: creating
	// a context on a card in use can disturb what runs there, and a card
	// that is serving has already answered this question.
	Busy func(card Card) bool
	// Unavailable explains why no attempt can be made at all right now —
	// the driver is not answering, the runtime is missing — or returns "".
	// Checked once per round, because it is a property of the host, and
	// the round is then skipped: a host that cannot be asked says nothing
	// about any card, and each card keeps whatever the vendor tool said.
	// Try reports the same discovery through Inconclusive when it can only be
	// made by attempting the check.
	Unavailable func() string

	Interval time.Duration
	Timeout  time.Duration
	// MaxBackoffRounds caps the per-card failure backoff. One limits the wait
	// to a single interval.
	MaxBackoffRounds int
	// FirstRound is how long to wait before the first round. A node that
	// just connected should not wait a quarter hour to learn its cards
	// cannot be used, but it must wait for the control plane to say what
	// it is running: Busy calls every card busy until it knows, so a
	// round fired at registration skipped all of them and the real first
	// round came an interval later. Measured on gpu3: the desired state
	// arrived about ten seconds after the agent registered.
	FirstRound time.Duration
	Now        func() time.Time
}

// Runner runs rounds and keeps each card's last answer.
type Runner struct {
	cfg Config

	mu       sync.Mutex
	outcomes map[string]Outcome
	// running guards against rounds overlapping. A round on a slow driver
	// can outlast the interval, and starting another would put a second
	// process on the same driver lock — the pile-up this package exists to
	// avoid.
	running bool
	// stuck are the cards whose last check was abandoned at the deadline
	// and has not returned since. The process behind it is still waiting
	// on the driver; starting another would add a second one to the same
	// wait every round, which is the pile-up this package exists to avoid.
	stuck map[string]bool
	// nextAttempt is when a failed card may be tried again.
	nextAttempt map[string]time.Time
	// roundStart is when the round in flight began, so a long round does not
	// stretch the backoff of cards tried near its end.
	roundStart time.Time
	// skipped is why the last round asked no card at all, or "".
	skipped string
}

func New(cfg Config) *Runner {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.FirstRound <= 0 {
		cfg.FirstRound = DefaultFirstRound
	}
	if cfg.MaxBackoffRounds <= 0 {
		cfg.MaxBackoffRounds = DefaultMaxBackoffRounds
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg, outcomes: map[string]Outcome{}, stuck: map[string]bool{}, nextAttempt: map[string]time.Time{}}
}

// Run keeps running rounds until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	first := time.NewTimer(r.cfg.FirstRound)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
		r.RunOnce(ctx)
	}
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce tries cards in order and returns when the round is done or an
// attempt makes starting another unsafe. A round already in flight is left to
// finish and this call does nothing.
func (r *Runner) RunOnce(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.roundStart = r.cfg.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	cards := r.cfg.Cards(ctx)
	if len(cards) == 0 {
		return
	}
	reason := r.unavailable()
	if reason == "" {
		for _, card := range cards {
			if ctx.Err() != nil {
				break
			}
			if stopped := r.try(ctx, card); stopped != "" {
				reason = stopped
				break
			}
		}
	}
	r.mu.Lock()
	changed := reason != r.skipped
	r.skipped = reason
	r.mu.Unlock()
	if reason != "" && changed {
		// Once per change: the round is skipped every quarter hour for as
		// long as the cause lasts.
		slog.Info("accelerator runtime check cannot run", "reason", reason, "cards", len(cards))
	}
}

// try asks about one card and records what it answered. It returns why the
// round must stop: a host-wide inconclusive answer, a timed-out attempt, or an
// older attempt that has not returned.
func (r *Runner) try(ctx context.Context, card Card) string {
	r.mu.Lock()
	stuck := r.stuck[card.Ident]
	anyStuck := len(r.stuck) > 0
	failures := r.outcomes[card.Ident].Failures
	r.mu.Unlock()
	if anyStuck {
		detail := fmt.Sprintf("an earlier runtime check has not returned after more than %s; no new card can be checked", r.cfg.Timeout)
		if stuck {
			detail = fmt.Sprintf("the previous runtime check of this card has not returned after more than %s", r.cfg.Timeout)
			r.record(card.Ident, Outcome{At: r.cfg.Now(), Failures: failures, Detail: detail})
		}
		return detail
	}
	if r.busy(card) {
		// Serving work is itself the answer, so the card keeps whatever it
		// last said rather than being recorded as untried.
		return ""
	}
	if r.backingOff(card.Ident) {
		return ""
	}
	attempt, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	// The deadline is the runner's, not only the attempt's. A check that
	// ignores its context — a python stuck in D state on the very driver
	// this package exists to survive — would otherwise hold the round
	// open forever, and with it every round after it.
	type attemptAnswer struct {
		err        error
		finishedAt time.Time
	}
	done := make(chan attemptAnswer, 1)
	go func() {
		err := r.cfg.Try(attempt, card)
		// Sent before the mark is cleared, so the runner either sees the
		// answer or finds the mark already gone, never a mark left behind.
		done <- attemptAnswer{err: err, finishedAt: time.Now()}
		r.mu.Lock()
		delete(r.stuck, card.Ident)
		r.mu.Unlock()
	}()
	deadline, _ := attempt.Deadline()
	timeoutErr := fmt.Errorf("the runtime check did not finish within %s", r.cfg.Timeout)
	// When done and the deadline are both ready, select's random choice must
	// not decide the verdict. The attempt state and completion timestamp give
	// both paths the same answer; a returned attempt is never left marked stuck.
	grade := func(answer attemptAnswer) error {
		if attempt.Err() != nil || !answer.finishedAt.Before(deadline) {
			return timeoutErr
		}
		return answer.err
	}
	var err error
	select {
	case answer := <-done:
		err = grade(answer)
	case <-attempt.Done():
		err = timeoutErr
		r.mu.Lock()
		r.stuck[card.Ident] = true
		r.mu.Unlock()
		// The check may have returned between the deadline and this mark.
		select {
		case answer := <-done:
			err = grade(answer)
			r.mu.Lock()
			delete(r.stuck, card.Ident)
			r.mu.Unlock()
		default:
		}
	}
	if inconclusive := inconclusiveReason(err); inconclusive != "" {
		// A host failure cannot push a card into a backoff it did not earn.
		return inconclusive
	}
	outcome := Outcome{OK: err == nil, At: r.cfg.Now()}
	if err != nil {
		outcome.Detail = err.Error()
		outcome.Failures = failures + 1
	}
	r.record(card.Ident, outcome)
	r.scheduleNext(card.Ident, outcome)
	if err == timeoutErr {
		return timeoutErr.Error()
	}
	return ""
}

func (r *Runner) backingOff(ident string) bool {
	r.mu.Lock()
	due, waiting := r.nextAttempt[ident]
	r.mu.Unlock()
	return waiting && r.cfg.Now().Before(due)
}

// scheduleNext doubles the wait after each consecutive failure up to
// MaxBackoffRounds intervals. A successful answer clears it.
func (r *Runner) scheduleNext(ident string, outcome Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if outcome.OK || outcome.Failures <= 0 {
		delete(r.nextAttempt, ident)
		return
	}
	rounds := 1
	for i := 1; i < outcome.Failures && rounds < r.cfg.MaxBackoffRounds; i++ {
		rounds *= 2
	}
	if rounds > r.cfg.MaxBackoffRounds {
		rounds = r.cfg.MaxBackoffRounds
	}
	from := r.roundStart
	if from.IsZero() {
		from = outcome.At
	}
	// Half an interval of slack, measured from the round rather than the
	// attempt, keeps a tick that arrives a little early from adding a round.
	r.nextAttempt[ident] = from.Add(time.Duration(rounds)*r.cfg.Interval - r.cfg.Interval/2)
}

// Skipped is why the last round could not ask any card or stopped partway,
// and "" when every card was asked. Doctor shows it: a check that has never
// run says nothing, and an operator has to be told that rather than read the
// silence as a pass.
func (r *Runner) Skipped() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skipped
}

// Outcomes is each card's last answer, by identity.
func (r *Runner) Outcomes() map[string]Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Outcome, len(r.outcomes))
	for ident, outcome := range r.outcomes {
		out[ident] = outcome
	}
	return out
}

func (r *Runner) record(ident string, outcome Outcome) {
	r.mu.Lock()
	r.outcomes[ident] = outcome
	r.mu.Unlock()
}

func (r *Runner) busy(card Card) bool {
	return r.cfg.Busy != nil && r.cfg.Busy(card)
}

func (r *Runner) unavailable() string {
	if r.cfg.Unavailable == nil {
		return ""
	}
	return r.cfg.Unavailable()
}
