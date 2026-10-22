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
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Defaults. One round every quarter hour costs each card a fraction of a
// second and catches a driver that broke since the last one; a card busy
// serving is skipped, so the cost does not land on a working node.
const (
	DefaultInterval = 15 * time.Minute
	DefaultTimeout  = 30 * time.Second
	// DefaultFirstRound leaves the control plane time to say what this
	// node runs, so the first round is not skipped for every card.
	DefaultFirstRound = 30 * time.Second
)

// Card is what the runner needs to know about one accelerator: how to name
// it, how to ask the runtime for it alone, and how a caller refers to it.
type Card struct {
	// Ident is the card's stable identity (accelerator.CardIdent), which is
	// how its answer is remembered and matched back to it.
	Ident string
	// Visible is what the vendor runtime wants in order to expose this card
	// and no other (for NVIDIA, the value of CUDA_VISIBLE_DEVICES).
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
	Unavailable func() string

	Interval time.Duration
	Timeout  time.Duration
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg, outcomes: map[string]Outcome{}, stuck: map[string]bool{}}
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

// RunOnce tries every card once, in order, and returns when the round is
// done. A round already in flight is left to finish and this call does
// nothing.
func (r *Runner) RunOnce(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
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
	r.mu.Lock()
	changed := reason != r.skipped
	r.skipped = reason
	r.mu.Unlock()
	if reason != "" {
		if changed {
			// Once per change: the round is skipped every quarter hour for
			// as long as the cause lasts.
			slog.Info("accelerator runtime check cannot run", "reason", reason, "cards", len(cards))
		}
		return
	}
	for _, card := range cards {
		if ctx.Err() != nil {
			return
		}
		r.try(ctx, card)
	}
}

func (r *Runner) try(ctx context.Context, card Card) {
	if r.busy(card) {
		// Serving work is itself the answer, so the card keeps whatever it
		// last said rather than being recorded as untried.
		return
	}
	r.mu.Lock()
	stuck := r.stuck[card.Ident]
	r.mu.Unlock()
	if stuck {
		r.record(card.Ident, Outcome{At: r.cfg.Now(),
			Detail: fmt.Sprintf("the previous runtime check of this card has not returned after more than %s", r.cfg.Timeout)})
		return
	}
	attempt, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	// The deadline is the runner's, not only the attempt's. A check that
	// ignores its context — a python stuck in D state on the very driver
	// this package exists to survive — would otherwise hold the round
	// open forever, and with it every round after it.
	done := make(chan error, 1)
	go func() {
		// Sent before the mark is cleared, so the runner either sees the
		// answer or finds the mark already gone, never a mark left behind.
		done <- r.cfg.Try(attempt, card)
		r.mu.Lock()
		delete(r.stuck, card.Ident)
		r.mu.Unlock()
	}()
	var err error
	select {
	case err = <-done:
	case <-attempt.Done():
		err = fmt.Errorf("the runtime check did not finish within %s", r.cfg.Timeout)
		r.mu.Lock()
		r.stuck[card.Ident] = true
		r.mu.Unlock()
		// The check may have returned between the deadline and this mark.
		select {
		case <-done:
			r.mu.Lock()
			delete(r.stuck, card.Ident)
			r.mu.Unlock()
		default:
		}
	}
	outcome := Outcome{OK: err == nil, At: r.cfg.Now()}
	if err != nil {
		outcome.Detail = err.Error()
	}
	r.record(card.Ident, outcome)
}

// Skipped is why the last round could not ask any card, or "" when it
// could. Doctor shows it: a check that has never run says nothing, and an
// operator has to be told that rather than read the silence as a pass.
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
