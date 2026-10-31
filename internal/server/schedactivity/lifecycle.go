package schedactivity

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// replayReach is how far back the replay reads: the span and an hour, so
// that a day's first bucket, aligned back to its quarter hour, is whole.
const replayReach = Span + time.Hour

// Replay fills the memory from the event log once (store.ReplaySchedulingActivity)
// and makes it ready: the events observed meanwhile are counted after it,
// none twice. It returns how many events it read and how many observed
// ones were dropped while it ran.
func (s *Service) Replay(now time.Time) (read int, dropped int, err error) {
	through, err := s.Store.LastEventID()
	if err != nil {
		return 0, 0, err
	}
	since := now.Add(-replayReach)
	count := func(evs []store.ObservedEvent) {
		read += len(evs)
		s.Tracker.Replayed(evs)
	}
	err = s.Store.ReplaySchedulingActivity(store.ActivityReplay{Since: since, ThroughID: through, Scheduling: count, Residency: count}, now)
	if err != nil {
		// What the replay counted stays; the memory counts from now on.
		dropped = s.Tracker.Ready(since.UnixMilli(), through)
		return read, dropped, err
	}
	return read, s.Tracker.Ready(since.UnixMilli(), through), nil
}

// Hour is one hour of the memory as stats.db keeps it, the deployments'
// presence and names read from the store; now bounds an hour in progress.
func (s *Service) Hour(hourStart int64, now time.Time) (statsdb.SchedulingHour, error) {
	end := min(hourStart+hourMS, now.UnixMilli())
	out := statsdb.SchedulingHour{HourMS: hourStart, Reasons: map[string]int64{}}
	counts := [][]int64{make([]int64, reasonCount)}
	s.Tracker.AddReasons(hourStart, hourStart+hourMS, hourStart, hourMS, counts)
	for i, r := range store.ActivityReasons() {
		if counts[0][i] > 0 {
			out.Reasons[string(r)] = counts[0][i]
		}
	}
	cards := map[store.EventCard]int64{}
	s.Tracker.AddCards(hourStart, hourStart+hourMS, cards)
	for c, n := range cards {
		out.Cards = append(out.Cards, statsdb.SchedulingCard{NodeUUID: c.NodeUUID, GPU: c.GPU, Switches: n})
	}
	from, to := time.UnixMilli(hourStart).UTC(), time.UnixMilli(end).UTC()
	deps := s.Tracker.Deployments(from, to, nil)
	ids := make([]uint, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	infos, err := s.Store.ResidencyDeployments(ids)
	if err != nil {
		return out, err
	}
	for id, d := range deps {
		info, ok := infos[id]
		if !ok || !store.ListedInSharing(info, d.ResidencySwitches) {
			continue
		}
		start, stop := store.Presence(info, d.HeldFrom, d.HeldTo, from, to)
		out.Deployments = append(out.Deployments, statsdb.SchedulingDeployment{
			DeploymentID: id, AwakeSeconds: roundSeconds(d.AwakeMS), PresentSeconds: roundSeconds(stop.Sub(start).Milliseconds()),
			Yielded: int64(d.Yielded), YieldedTo: int64(d.YieldedTo), Wakes: int64(d.Wakes),
		})
	}
	return out, nil
}

// roundSeconds is a length in ms as whole seconds, 0 when it is not positive.
func roundSeconds(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return (ms + 500) / 1000
}

// Flush writes every hour the memory holds whole that is over and not
// written yet, in one transaction; final also writes the hour in progress
// (the server stops), which the next run's replay counts again and
// rewrites. Nothing is written before the replay is in.
func (s *Service) Flush(ctx context.Context, final bool) error {
	if s.Stats == nil || !s.Tracker.IsReady() {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	now := s.Now()
	current := floorTo(now.UnixMilli(), hourMS)
	first := max(s.flushedThrough, ceilTo(s.Tracker.StartMS(), hourMS))
	end := current
	if final {
		end = current + hourMS
	}
	var hours []statsdb.SchedulingHour
	for h := first; h < end; h += hourMS {
		hour, err := s.Hour(h, now)
		if err != nil {
			return err
		}
		hours = append(hours, hour)
	}
	if err := s.Stats.ReplaceSchedulingHours(ctx, hours); err != nil {
		return err
	}
	if len(hours) > 0 {
		// Once an hour: stop following instances deleted without a removal
		// event, then drop what has aged out.
		readAt := s.Now()
		live, err := s.Store.InstanceUUIDs()
		if err != nil {
			return err
		}
		s.Tracker.Forget(live, readAt, now)
		s.Tracker.Prune(now)
	}
	s.flushedThrough = max(s.flushedThrough, current)
	return nil
}
