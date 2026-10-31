package schedactivity

import (
	"context"
	"sort"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// MaxBuckets is about the most buckets a range is split into.
const MaxBuckets = 200

// steps are the bucket lengths a range may be split into, shortest first.
var steps = []time.Duration{
	time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// Service answers the scheduling activity from the memory and, for what is
// older, stats.db (api.SchedulingActivity).
type Service struct {
	Tracker *Tracker
	Store   *store.Store
	// Stats is stats.db; nil answers from the memory alone.
	Stats *statsdb.Store
	// Retention is the data retention setting in effect.
	Retention func() store.DataRetention
	Now       func() time.Time

	// flushedThrough is the end of the last hour written (Unix ms), under
	// flushMu.
	flushMu        sync.Mutex
	flushedThrough int64
}

// coverage is where each source begins at now.
type coverage struct {
	memFrom    int64 // the first minute the memory holds whole
	hourlyFrom int64 // the hour rows' retention
	dailyFrom  int64 // the day rows' retention
	loc        *time.Location
}

func (s *Service) coverage(now time.Time) coverage {
	nowMS := now.UnixMilli()
	memFrom := max(s.Tracker.StartMS(), nowMS-(ringMinutes-2)*minuteMS)
	memFrom = ceilTo(memFrom, minuteMS)
	c := coverage{memFrom: memFrom, hourlyFrom: memFrom, dailyFrom: memFrom, loc: time.UTC}
	if s.Retention != nil {
		r := s.Retention()
		if loc, err := r.Location(); err == nil {
			c.loc = loc
		}
		if s.Stats != nil {
			c.hourlyFrom = min(memFrom, nowMS-int64(r.StatsHourlyDays)*24*int64(time.Hour/time.Millisecond))
			c.dailyFrom = min(c.hourlyFrom, nowMS-int64(r.StatsDailyDays)*24*int64(time.Hour/time.Millisecond))
		}
	}
	return c
}

func ceilTo(v, step int64) int64 {
	if r := v % step; r != 0 {
		return v - r + step
	}
	return v
}

func floorTo(v, step int64) int64 { return v - v%step }

const hourMS = int64(time.Hour / time.Millisecond)

// plan is how a range is read: its buckets and the part each source covers.
type plan struct {
	align, step, to int64
	n               int
	memFrom         int64 // memory from here
	hoursFrom       int64 // hour rows over [hoursFrom, memFrom)
	daysFrom        int64 // day rows over [daysFrom, hoursFrom)
	beyond          bool
}

// planRange picks the bucket length (step, or the shortest that keeps the
// range within MaxBuckets when step is 0, never shorter than the oldest
// source the range reaches keeps) and the part of the range each source
// answers.
func (s *Service) planRange(from, to time.Time, step time.Duration, c coverage) plan {
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()
	resolution := time.Minute
	switch {
	case fromMS < c.hourlyFrom:
		resolution = 24 * time.Hour
	case fromMS < c.memFrom:
		resolution = time.Hour
	}
	if step == 0 {
		step = steps[len(steps)-1]
		for _, candidate := range steps {
			if candidate >= resolution && (toMS-fromMS+candidate.Milliseconds()-1)/candidate.Milliseconds() <= MaxBuckets {
				step = candidate
				break
			}
		}
	}
	step = max(step, resolution)
	p := plan{step: step.Milliseconds(), to: toMS, beyond: fromMS < c.dailyFrom}
	if step%(24*time.Hour) == 0 {
		day := rollup.DayOf(from, c.loc)
		p.align, _ = day.Bounds(c.loc)
	} else {
		p.align = floorTo(fromMS, p.step)
	}
	p.n = int((toMS - p.align + p.step - 1) / p.step)
	p.memFrom = c.memFrom
	if p.step >= hourMS {
		p.memFrom = ceilTo(c.memFrom, hourMS)
	}
	p.memFrom = max(p.memFrom, p.align)
	p.hoursFrom, p.daysFrom = p.memFrom, p.memFrom
	if s.Stats != nil && p.align < p.memFrom {
		p.hoursFrom = max(p.align, ceilTo(c.hourlyFrom, hourMS))
		if p.step%(24*hourMS) == 0 && p.align < p.hoursFrom {
			// Days before the hour rows' retention come from the day rows;
			// the first day whole within it starts the hours.
			cut := rollup.DayOf(time.UnixMilli(p.hoursFrom), c.loc)
			if start, _ := cut.Bounds(c.loc); start < p.hoursFrom {
				cut = cut.Next()
			}
			p.hoursFrom, _ = cut.Bounds(c.loc)
			p.daysFrom = p.align
		} else {
			p.daysFrom = p.hoursFrom
		}
	}
	return p
}

// Activity is the activity over [from, to) in buckets of step (0: the
// server's choice), with the topN cards that changed hands most.
func (s *Service) Activity(ctx context.Context, from, to time.Time, step time.Duration, topN int) (dto.SchedulingActivity, error) {
	c := s.coverage(s.Now())
	p := s.planRange(from, to, step, c)
	reasons := store.ActivityReasons()
	counts := make([][]int64, p.n)
	for i := range counts {
		counts[i] = make([]int64, len(reasons))
	}
	cards := map[store.EventCard]int64{}
	s.Tracker.AddReasons(p.memFrom, p.to, p.align, p.step, counts)
	s.Tracker.AddCards(p.memFrom, p.to, cards)
	if err := s.addStored(ctx, p, c.loc, counts, cards); err != nil {
		return dto.SchedulingActivity{}, err
	}
	out := dto.SchedulingActivity{
		From: time.UnixMilli(p.align).UTC(), To: to.UTC(), BucketSeconds: p.step / 1000,
		Reasons: make([]dto.ActivityReason, len(reasons)), Buckets: make([]dto.ActivityBucket, p.n),
		TopCards: []dto.ActivityCard{}, BeyondRetention: p.beyond,
	}
	for i, r := range reasons {
		out.Reasons[i].Key = string(r)
	}
	for i := range counts {
		out.Buckets[i] = dto.ActivityBucket{Start: time.UnixMilli(p.align + int64(i)*p.step).UTC(), Counts: counts[i]}
		for r, n := range counts[i] {
			out.Reasons[r].Count += n
		}
	}
	top, err := s.topCards(cards, topN)
	if err != nil {
		return dto.SchedulingActivity{}, err
	}
	out.TopCards = top
	return out, nil
}

// addStored adds the hour and day rows of the part of the range before the
// memory.
func (s *Service) addStored(ctx context.Context, p plan, loc *time.Location, counts [][]int64, cards map[store.EventCard]int64) error {
	if s.Stats == nil {
		return nil
	}
	add := func(at int64, reason string, n int64) {
		r := store.ActivityReasonIndex(store.ActivityReason(reason))
		if i := (at - p.align) / p.step; r >= 0 && i >= 0 && int(i) < len(counts) {
			counts[i][r] += n
		}
	}
	if p.hoursFrom < p.memFrom {
		r := statsdb.Range{From: p.hoursFrom, To: p.memFrom}
		rows, err := s.Stats.SchedulingReasons(ctx, r)
		if err != nil {
			return err
		}
		for _, row := range rows {
			add(row.Bucket, row.Reason, row.Events)
		}
		if err := addCards(ctx, s.Stats, r, cards); err != nil {
			return err
		}
	}
	if p.daysFrom < p.hoursFrom {
		r := statsdb.Range{Daily: true, From: int64(rollup.DayOf(time.UnixMilli(p.daysFrom), loc)), To: int64(rollup.DayOf(time.UnixMilli(p.hoursFrom), loc))}
		rows, err := s.Stats.SchedulingReasons(ctx, r)
		if err != nil {
			return err
		}
		for _, row := range rows {
			start, _ := rollup.Day(row.Bucket).Bounds(loc)
			add(start, row.Reason, row.Events)
		}
		if err := addCards(ctx, s.Stats, r, cards); err != nil {
			return err
		}
	}
	return nil
}

func addCards(ctx context.Context, stats *statsdb.Store, r statsdb.Range, into map[store.EventCard]int64) error {
	rows, err := stats.SchedulingCards(ctx, r)
	if err != nil {
		return err
	}
	for _, c := range rows {
		into[store.EventCard{NodeUUID: c.NodeUUID, GPU: c.GPU}] += c.Switches
	}
	return nil
}

// topCards are the n cards with the most switches, named.
func (s *Service) topCards(cards map[store.EventCard]int64, n int) ([]dto.ActivityCard, error) {
	list := make([]dto.ActivityCard, 0, len(cards))
	for c, switches := range cards {
		if switches > 0 {
			list = append(list, dto.ActivityCard{NodeUUID: c.NodeUUID, GPUIndex: c.GPU, Switches: switches})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Switches != list[j].Switches {
			return list[i].Switches > list[j].Switches
		}
		if list[i].NodeUUID != list[j].NodeUUID {
			return list[i].NodeUUID < list[j].NodeUUID
		}
		return list[i].GPUIndex < list[j].GPUIndex
	})
	if len(list) > n {
		list = list[:n]
	}
	if len(list) == 0 || s.Store == nil {
		return list, nil
	}
	uuids := make([]string, 0, len(list))
	for _, c := range list {
		uuids = append(uuids, c.NodeUUID)
	}
	names, _, err := s.Store.EventNodeRefs(uuids, nil)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Node = names[list[i].NodeUUID].Name
	}
	return list, nil
}

// Deployments is each deployment's figures over [from, to) from the memory
// (store.DeploymentActivity); want, when not nil, limits it to some.
func (s *Service) Deployments(from, to time.Time, want map[uint]bool) map[uint]store.DeploymentActivity {
	return s.Tracker.Deployments(from, to, want)
}
