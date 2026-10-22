package monitorapi

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// MaxSeriesCount and MaxSeriesPoints cap one /monitor/series response: a
// request whose result would exceed either is refused with
// Deps.SeriesTooLarge (api.go maps that to 400 with a stable code) rather
// than silently truncated, which would let a chart render as if it were
// complete while quietly missing data.
const (
	MaxSeriesCount  = 64
	MaxSeriesPoints = 20000
)

// buildSeries serves the GET /api/v1/monitor/series metric browser (plan
// §6): a raw tsdb.QueryRange plus one of monitor's exported aggregate
// helpers, grouped by the by label (empty groups every series into one).
func (s *Service) buildSeries(ctx context.Context, metric, by, fn string, rng, step time.Duration) ([]dto.MonitorSeries, error) {
	if s.deps.TSDB == nil {
		return []dto.MonitorSeries{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := s.deps.now()
	startMS := now.Add(-rng).UnixMilli()
	endMS := now.UnixMilli()
	stepMS := step.Milliseconds()
	series, err := s.deps.TSDB.QueryRange(metric, nil, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	grouped := groupSeriesBy(series, by, fn)

	if len(grouped) > MaxSeriesCount {
		return nil, s.tooLarge()
	}
	totalPoints := 0
	for _, g := range grouped {
		totalPoints += len(g.Points)
	}
	if totalPoints > MaxSeriesPoints {
		return nil, s.tooLarge()
	}

	out := make([]dto.MonitorSeries, 0, len(grouped))
	for _, g := range grouped {
		labels := g.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, dto.MonitorSeries{Labels: labels, Points: pointsFromTSDB(g.Points)})
	}
	return out, nil
}

// tooLarge answers Deps.SeriesTooLarge, falling back to a local sentinel if
// the app layer somehow left it nil - buildSeries must never panic on a nil
// error value, and a caller who forgets to wire it still gets *an* error
// rather than a silently-accepted response the caps were meant to prevent.
func (s *Service) tooLarge() error {
	if s.deps.SeriesTooLarge != nil {
		return s.deps.SeriesTooLarge
	}
	return errSeriesTooLargeFallback
}

// groupSeriesBy applies fn's aggregation, grouped by the by label (by=""
// combines every input series into one). fn is always one of
// avg/sum/max/rate by the time api.monitorSeries calls this (empty defaults
// to avg there), so the default case is unreachable in production and exists
// only so an unrecognized fn cannot fall through silently.
//
// rate is computed per raw series first, then summed within each group -
// "sum(rate(x)) by (label)" - so a counter reset on one underlying series
// cannot corrupt another series sharing its group.
//
// by=="gpu" is special-cased: grouping directly on the raw "gpu" label (a
// bare per-node index) would merge two different nodes' index-0 card into
// one series, since the index alone does not identify a physical GPU across
// a fleet. withGPUGroupingKey rewrites it to the card's own gpu_uuid first,
// falling back to "<node>/<index>" only when an old agent's sample carries
// no gpu_uuid.
func groupSeriesBy(series []tsdb.Series, by, fn string) []tsdb.Series {
	if by == "gpu" {
		series = withGPUGroupingKey(series)
	}
	switch fn {
	case "sum":
		return monitor.SumBy(series, by)
	case "max":
		return monitor.MaxBy(series, by)
	case "rate":
		return monitor.SumBy(monitor.Rate(series), by)
	default:
		return monitor.AvgBy(series, by)
	}
}

// withGPUGroupingKey rewrites each series' "gpu" label to a value that
// uniquely identifies the physical card across the whole fleet.
func withGPUGroupingKey(series []tsdb.Series) []tsdb.Series {
	out := make([]tsdb.Series, len(series))
	for i, ser := range series {
		key := ser.Labels["gpu_uuid"]
		if key == "" {
			key = ser.Labels["node"] + "/" + ser.Labels["gpu"]
		}
		labels := make(map[string]string, len(ser.Labels))
		for k, v := range ser.Labels {
			labels[k] = v
		}
		labels["gpu"] = key
		out[i] = tsdb.Series{Labels: labels, Points: ser.Points}
	}
	return out
}

// errSeriesTooLargeFallback backstops tooLarge; see its doc comment. It is
// unexported and only ever surfaces if Deps.SeriesTooLarge is left nil,
// which app.go's wiring never does - api.go's own sentinel is what actually
// reaches the wire.
type seriesTooLargeFallback struct{}

func (seriesTooLargeFallback) Error() string { return "monitorapi: series request too large" }

var errSeriesTooLargeFallback error = seriesTooLargeFallback{}
