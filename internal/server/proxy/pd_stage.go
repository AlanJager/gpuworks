package proxy

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// pdStage is one request's PD prefill stage as the gateway runs it (ADR
// 0047): when it began, the stage's own span (the prefill copy's parent in
// the trace), the prefill instance that answered, how long the gateway
// waited for it, what that instance said of its own queue and prefill, and
// why the request was decoded alone when it was. The concurrent handoff
// runs it in the background and sets only the instance and the fallback.
type pdStage struct {
	started time.Time
	span    tracecontext.SpanID

	mu        sync.Mutex
	instance  string
	waited    time.Duration // < 0: the gateway did not wait
	queueMS   int32
	computeMS int32
	fallback  string
	// cached is the prompt the prefill instance served from its cache, by
	// its reply's usage; cachedKnown says the reply stated it.
	cached      uint64
	cachedKnown bool
}

func newPDStage(started time.Time) *pdStage {
	return &pdStage{started: started, span: tracecontext.NewSpanID(), waited: -1, queueMS: uxrecord.Unknown, computeMS: uxrecord.Unknown}
}

// tried notes the prefill instance the stage is sending the copy to.
func (s *pdStage) tried(instance string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.instance = instance
	s.mu.Unlock()
}

// answered notes the instance that took the copy and its own account of it:
// the reply's request stats, read by the deployment's rules, and its
// prompt-cache figure.
func (s *pdStage) answered(instance string, stats map[string]float64, cached uint64, cachedKnown bool) {
	if s == nil {
		return
	}
	ms := func(stat string) int32 {
		v, ok := stats[stat]
		if !ok {
			return uxrecord.Unknown
		}
		return int32(min(max(math.Round(v*1000), 0), math.MaxInt32))
	}
	s.mu.Lock()
	s.instance = instance
	s.queueMS = ms(inferencemetrics.RequestQueueSeconds)
	s.computeMS = ms(inferencemetrics.RequestPrefillSeconds)
	s.cached, s.cachedKnown = cached, cachedKnown
	s.mu.Unlock()
}

// prefillCached is the prompt the prefill instance's cache served, for the
// decode reply to report (cached_rewrite.go); ok is false when no prefill
// instance answered with the figure - a fallback, the concurrent handoff,
// a reply without it.
func (s *pdStage) prefillCached() (cached uint64, ok bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached, s.cachedKnown && s.fallback == "" && s.instance != ""
}

// fellBack notes that the request goes to the decoder alone, and why; no
// prefill instance took it.
func (s *pdStage) fellBack(reason string) {
	s.mu.Lock()
	s.instance, s.fallback, s.cachedKnown = "", reason, false
	s.mu.Unlock()
}

// done notes that the gateway's wait for the stage ended now.
func (s *pdStage) done(now time.Time) {
	s.mu.Lock()
	s.waited = now.Sub(s.started)
	s.mu.Unlock()
}

// prefillInstance is the instance the stage is with or went to.
func (s *pdStage) prefillInstance() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instance
}

// record is the stage as the request's record keeps it, its start relative
// to receivedAt.
func (s *pdStage) record(receivedAt time.Time) *uxrecord.PDStage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &uxrecord.PDStage{
		PrefillInstance: s.instance, PrefillMS: uxrecord.Unknown,
		PrefillQueueMS: s.queueMS, PrefillComputeMS: s.computeMS, Fallback: s.fallback,
		SpanID: s.span, StartMS: clampMS(s.started.Sub(receivedAt).Milliseconds()),
	}
	if s.waited >= 0 {
		out.PrefillMS = clampMS(s.waited.Milliseconds())
	} else {
		out.SpanID = tracecontext.SpanID{} // no stage the trace could show
	}
	return out
}

// setPrefillTraceHeaders makes the prefill copy carry the stage's span as
// its parent, so the prefill engine's own span (when it exports one) nests
// under the stage in the gateway's trace.
func setPrefillTraceHeaders(ctx context.Context, h http.Header, stage *pdStage) {
	id, ok := ctx.Value(identityKey).(requestIdentity)
	if !ok || stage == nil {
		setTraceHeaders(ctx, h)
		return
	}
	h.Set(traceparentHeader, tracecontext.Format(id.trace.Trace, stage.span, id.trace.Flags))
	if id.trace.State != "" {
		h.Set(tracestateHeader, id.trace.State)
	}
}
