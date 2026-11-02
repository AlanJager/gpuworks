package proxy

import (
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// The wake-hold wait series (inferencemetrics "wake_hold_wait_ms"): how long
// each request the gateway held for a sleeping deployment waited, from being
// held to being served, refused or abandoned. It is accumulated per
// deployment beside the other gateway histograms and flushed with them, so a
// held request costs one in-memory observation and no write.
var (
	metricHoldWaitBucket = inferencemetrics.Gateway("wake_hold_wait_ms").Series() + "_bucket"
	metricHoldWaitSum    = inferencemetrics.Gateway("wake_hold_wait_ms").Series() + "_sum"
	metricHoldWaitCount  = inferencemetrics.Gateway("wake_hold_wait_ms").Series() + "_count"
)

// holdWaitBoundsMS reach 30 minutes, so a request that ran out of a long
// hold lands in a bucket of its own rather than in +Inf (a yield policy's
// hold_max_sec allows up to an hour; only holds past 30 minutes reach +Inf).
var holdWaitBoundsMS = []float64{500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 300000, 600000, 1800000}

// RecordHoldWait counts one held request's wait. The deployment becomes dirty,
// so the next flush writes the histogram.
func (a *Accountant) RecordHoldWait(deploymentID uint, wait time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	c := a.deploymentLocked(deploymentID)
	c.response.holdWait.observe(holdWaitBoundsMS, float64(wait)/float64(time.Millisecond))
	c.dirty = true
	a.mu.Unlock()
}

// holdWaitSamples emits the histogram once a request has been held.
func (r *responseCounters) holdWaitSamples(labels map[string]string, tms int64) []tsdb.Sample {
	if r.holdWait.count == 0 {
		return nil
	}
	out := r.holdWait.bucketSamples(metricHoldWaitBucket, holdWaitBoundsMS, labels, tms)
	return append(out,
		tsdb.Sample{Metric: metricHoldWaitSum, Labels: labels, Value: r.holdWait.sum, TimeMS: tms},
		tsdb.Sample{Metric: metricHoldWaitCount, Labels: labels, Value: float64(r.holdWait.count), TimeMS: tms},
	)
}

// leaveHold ends a request's hold and counts how long it lasted.
func (p *Proxy) leaveHold(deploymentID uint, req *heldRequest) {
	wait := p.hold.now().Sub(req.since)
	p.hold.leave(deploymentID, req)
	p.acct.RecordHoldWait(deploymentID, wait)
}
