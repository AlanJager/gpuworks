package slurm

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Sweep returns the cards no instance uses any more and tells the engine
// about instances whose job Slurm ended. inUse is the cards each instance
// holds (engine.InstanceCards); lost stops an instance.
func (l *Leaser) Sweep(ctx context.Context, inUse map[string][]string, lost func(instance, message string)) error {
	l.mu.Lock()
	before := map[string]*holdJob{}
	for id, j := range l.jobs {
		if j.running {
			before[id] = j
		}
	}
	if err := l.refreshOwnLocked(ctx); err != nil {
		l.mu.Unlock()
		return err
	}
	used := map[string]bool{}
	for _, cards := range inUse {
		for _, c := range cards {
			used[c] = true
		}
	}
	type gone struct{ instance, message string }
	var ended []gone
	for id, j := range before {
		if _, still := l.jobs[id]; still {
			continue
		}
		for instance, cards := range inUse {
			if overlaps(cards, j.cards) {
				ended = append(ended, gone{instance, fmt.Sprintf("Slurm job %s holding the instance's cards ended%s", id, l.endReason(ctx, id))})
			}
		}
	}
	now := l.now()
	for id, j := range l.jobs {
		if j.waiting {
			continue // a borrow waits for it to start, and hands its cards on
		}
		if !j.running {
			// Nothing waits for it: it is left over from before a restart,
			// or its start gave up.
			l.cancelLocked(ctx, id, "pending with nobody waiting")
			continue
		}
		if overlaps(j.cards, keys(used)) || (j.adopted && !l.leased && now.Sub(l.startedAt) < adoptedGrace) {
			j.idle = time.Time{}
			continue
		}
		if j.idle.IsZero() {
			j.idle = now
			continue
		}
		if now.Sub(j.idle) >= l.settings().IdleReturn {
			l.cancelLocked(ctx, id, "idle")
		}
	}
	l.mu.Unlock()
	for _, g := range ended {
		lost(g.instance, g.message)
	}
	return nil
}

// endReason is how Slurm says a job ended, while it still remembers it.
func (l *Leaser) endReason(ctx context.Context, id string) string {
	record, err := l.jobRecord(ctx, id)
	if err != nil {
		return ""
	}
	f := fields(record)
	if f["JobState"] == "" {
		return ""
	}
	return fmt.Sprintf(" (%s, %s)", f["JobState"], f["Reason"])
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Run sweeps every interval until ctx ends.
func (l *Leaser) Run(ctx context.Context, interval time.Duration, inUse func() map[string][]string, lost func(instance, message string)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Sweep(ctx, inUse(), lost); err != nil {
				l.log.Debug("Slurm placeholder sweep failed; retrying", "err", err)
			}
		}
	}
}
