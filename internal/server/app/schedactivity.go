package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/schedactivity"
)

// The scheduling activity (internal/server/schedactivity) is counted beside
// the other long-term statistics: the store hands it every scheduling and
// residency event once written, the replay fills the last 24 hours from the
// event log once on start, and the statistics loop writes every finished
// hour to stats.db, and the hour in progress when the server stops.

// newSchedulingActivity makes the activity and hooks it to the store's event
// writes before anything is written, so the replay and the events observed
// meanwhile leave nothing out.
func (o *opsStats) newSchedulingActivity() {
	o.sched = &schedactivity.Service{
		Tracker: schedactivity.NewTracker(), Store: o.store, Stats: o.stats, Retention: o.Retention,
		Now: func() time.Time { return o.now() },
	}
	o.store.SetEventObserver(o.sched.Tracker.Observe)
}

// replayScheduling fills the activity's memory from the event log once.
func (o *opsStats) replayScheduling(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	started := time.Now()
	read, dropped, err := o.sched.Replay(o.now())
	if err != nil {
		o.log.Warn("scheduling activity replay failed; it counts from now on", "err", err, "events", read)
		return
	}
	o.log.Info("scheduling activity replayed", "events", read, "dropped_while_replaying", dropped, "took", time.Since(started).Round(time.Millisecond))
}

// flushScheduling writes the activity's finished hours (and, final, the one
// in progress).
func (o *opsStats) flushScheduling(ctx context.Context, final bool) {
	if o.sched == nil {
		return
	}
	if err := o.sched.Flush(ctx, final); err != nil {
		o.log.Warn("scheduling activity flush failed; it is written with the next flush", "err", err)
	}
}
