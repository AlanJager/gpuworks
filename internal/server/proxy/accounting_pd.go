package proxy

import (
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// PD gateway metrics (ADR 0028): how long the prefill stage took, how often
// it failed, and how often a request fell back to the decoder alone, by
// reason. The prefill instance's own request is never a user request: it is
// not dispatched, finished or billed.
const (
	metricPDPrefillDurationSum   = "gpuworks_proxy_pd_prefill_duration_ms_sum"
	metricPDPrefillDurationCount = "gpuworks_proxy_pd_prefill_duration_ms_count"
	metricPDFallbackTotal        = "gpuworks_proxy_pd_fallback_total"
)

type pdCounters struct {
	prefillSumMs float64
	prefillCount uint64
	fallbacks    map[string]uint64
}

// RecordPDPrefill records one prefill stage; fallback is the reason it did
// not hand off, empty when it did.
func (a *Accountant) RecordPDPrefill(deploymentID uint, duration time.Duration, fallback string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.pdLocked(deploymentID)
	c.prefillSumMs += float64(duration.Microseconds()) / 1000
	c.prefillCount++
	a.deploymentLocked(deploymentID).dirty = true
}

// RecordPDFallback records one request the decoder served alone.
func (a *Accountant) RecordPDFallback(deploymentID uint, reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pdLocked(deploymentID).fallbacks[reason]++
	a.deploymentLocked(deploymentID).dirty = true
}

// PDFallbacks returns a deployment's fallback counts by reason.
func (a *Accountant) PDFallbacks(deploymentID uint) map[string]uint64 {
	out := map[string]uint64{}
	if a == nil {
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.pd[deploymentID]; ok {
		for reason, n := range c.fallbacks {
			out[reason] = n
		}
	}
	return out
}

func (a *Accountant) pdLocked(id uint) *pdCounters {
	if a.pd == nil {
		a.pd = map[uint]*pdCounters{}
	}
	c := a.pd[id]
	if c == nil {
		c = &pdCounters{fallbacks: map[string]uint64{}}
		a.pd[id] = c
	}
	return c
}

// pdSamplesLocked is the PD part of one deployment's flush.
func (a *Accountant) pdSamplesLocked(id uint, tms int64) []tsdb.Sample {
	c, ok := a.pd[id]
	if !ok {
		return nil
	}
	dep := strconv.FormatUint(uint64(id), 10)
	labels := func() map[string]string { return map[string]string{"deployment": dep, "node": serverNodeLabel} }
	out := []tsdb.Sample{
		{Metric: metricPDPrefillDurationSum, Labels: labels(), Value: c.prefillSumMs, TimeMS: tms},
		{Metric: metricPDPrefillDurationCount, Labels: labels(), Value: float64(c.prefillCount), TimeMS: tms},
	}
	for reason, n := range c.fallbacks {
		l := labels()
		l["reason"] = reason
		out = append(out, tsdb.Sample{Metric: metricPDFallbackTotal, Labels: l, Value: float64(n), TimeMS: tms})
	}
	return out
}
