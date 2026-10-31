package schedactivity

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Reading the memory. A range is of minutes: a minute is in [fromMS, toMS)
// when it starts there, so a caller aligns its range to minutes.

// minutesOf calls fn with every bucket of the minutes starting in
// [fromMS, toMS) that the ring holds.
func (t *Tracker) minutesOf(fromMS, toMS int64, fn func(start int64, b *bucket)) {
	first := (fromMS + minuteMS - 1) / minuteMS
	last := (toMS - 1) / minuteMS
	if last-first >= ringMinutes {
		first = last - ringMinutes + 1
	}
	for m := first; m <= last; m++ {
		b := &t.ring[m%ringMinutes]
		if b.minute == m {
			fn(m*minuteMS, b)
		}
	}
}

// AddReasons adds each minute's counts of [fromMS, toMS) into out: the
// bucket a minute falls in is (minute - alignMS) / stepMS, and out[i] has a
// count per reason (store.ActivityReasons).
func (t *Tracker) AddReasons(fromMS, toMS, alignMS, stepMS int64, out [][]int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.minutesOf(fromMS, toMS, func(start int64, b *bucket) {
		i := (start - alignMS) / stepMS
		if i < 0 || int(i) >= len(out) {
			return
		}
		for r, n := range b.reasons {
			out[i][r] += n
		}
	})
}

// AddCards adds each card's switches over [fromMS, toMS) into out.
func (t *Tracker) AddCards(fromMS, toMS int64, out map[store.EventCard]int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.minutesOf(fromMS, toMS, func(_ int64, b *bucket) {
		for _, c := range b.cards {
			out[store.EventCard{NodeUUID: t.nodeNames[c.node], GPU: int(c.gpu)}]++
		}
	})
}

// Deployments is each deployment's figures over [from, to): its yields,
// yields to it and wakes in the minutes starting in the range, how long it
// was awake and the span its instances held cards in. want, when not nil,
// limits it to some deployments. A deployment with nothing in the range is
// absent.
func (t *Tracker) Deployments(from, to time.Time, want map[uint]bool) map[uint]store.DeploymentActivity {
	t.mu.Lock()
	defer t.mu.Unlock()
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	out := map[uint]store.DeploymentActivity{}
	t.minutesOf(fromMS-fromMS%minuteMS, toMS, func(_ int64, b *bucket) {
		for _, h := range b.deps {
			id := uint(h.dep)
			if want != nil && !want[id] {
				continue
			}
			d := out[id]
			switch h.kind {
			case hitYielded:
				d.Yielded++
			case hitYieldedTo:
				d.YieldedTo++
			case hitWake:
				d.Wakes++
			}
			out[id] = d
		}
	})
	for id, dep := range t.deps {
		if want != nil && !want[id] {
			continue
		}
		awake, _, _ := dep.awake.within(fromMS, toMS)
		_, first, last := dep.held.within(fromMS, toMS)
		if awake == 0 && first == 0 {
			continue
		}
		d := out[id]
		d.AwakeMS = awake
		if first != 0 {
			d.HeldFrom, d.HeldTo = time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()
		}
		out[id] = d
	}
	return out
}
