package proxy

import (
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func (a *Accountant) snapshotAndClearDirty() ([]uint, []tsdb.Sample) {
	tms := a.clock().UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneRoutingSeriesLocked(time.UnixMilli(tms))
	var ids []uint
	var out []tsdb.Sample
	for id, c := range a.counters {
		if !c.dirty && c.inflight == 0 {
			continue
		}
		ids = append(ids, id)
		c.dirty = false
		dep := strconv.FormatUint(uint64(id), 10)
		for class, count := range c.classTotals {
			out = append(out, tsdb.Sample{
				Metric: metricRequestsTotal,
				Labels: map[string]string{"deployment": dep, "class": string(class), "node": serverNodeLabel},
				Value:  float64(count),
				TimeMS: tms,
			})
		}
		out = append(out,
			tsdb.Sample{
				Metric: metricDurationSum,
				Labels: map[string]string{"deployment": dep, "node": serverNodeLabel},
				Value:  c.durationSumMs,
				TimeMS: tms,
			},
			tsdb.Sample{
				Metric: metricDurationCount,
				Labels: map[string]string{"deployment": dep, "node": serverNodeLabel},
				Value:  float64(c.durationCount),
				TimeMS: tms,
			},
			tsdb.Sample{
				Metric: metricInflight,
				Labels: map[string]string{"deployment": dep, "node": serverNodeLabel},
				Value:  float64(c.inflight),
				TimeMS: tms,
			},
		)
		for key, count := range a.routed {
			prefix := dep + "\x00"
			if len(key) > len(prefix) && key[:len(prefix)] == prefix {
				out = append(out, tsdb.Sample{Metric: metricRoutedTotal, Labels: map[string]string{"deployment": dep, "instance": key[len(prefix):], "node": serverNodeLabel}, Value: float64(count), TimeMS: tms})
			}
		}
		out = append(out, c.response.samples(dep, tms)...)
		out = append(out, a.pdSamplesLocked(id, tms)...)
		for key, count := range a.retries {
			prefix := dep + "\x00"
			if len(key) > len(prefix) && key[:len(prefix)] == prefix {
				out = append(out, tsdb.Sample{Metric: metricRetriesTotal, Labels: map[string]string{"deployment": dep, "reason": key[len(prefix):], "node": serverNodeLabel}, Value: float64(count), TimeMS: tms})
			}
		}
	}
	return ids, out
}
