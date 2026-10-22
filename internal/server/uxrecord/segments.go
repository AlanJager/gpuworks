package uxrecord

import "math"

// Segments split a request's time into the phases a user waits through, in
// order. The gateway's own share (receipt to dispatch) is measured. After it
// the gateway sees one wait for the first token; the engine's per-request
// stats, when it reported them, split that wait into its queue and prefill and
// leave the rest as the engine's preprocessing: what it did before its queue
// started (rendering and tokenizing the prompt, waiting for the step it was
// running to end) and the trip of the first token back, network included.
// The segments of a request add up to its end-to-end time (or, in flight,
// to its age), which is what lets the latency breakdown average them: the
// means of the segments add up to the mean end-to-end time. This is the one
// place the split is made; the request view draws these segments and the
// breakdown sums them.

// SegmentKind names one phase.
type SegmentKind string

const (
	// SegmentGateway is the gateway's own share: receipt to dispatch, less
	// a PD request's prefill stage.
	SegmentGateway SegmentKind = "gateway"
	// SegmentPDPrefill is a PD request's prefill stage, which the gateway
	// waits for before it hands the request to the decode instance.
	SegmentPDPrefill SegmentKind = "pd_prefill"
	// SegmentPreprocess is the part of the wait the engine's account leaves
	// out when it reports its queue: the time before its queue started and
	// the trip of the first token back (for a reply that does not stream,
	// whatever the engine's queue, prefill and decode leave of it).
	SegmentPreprocess SegmentKind = "preprocess"
	SegmentQueue      SegmentKind = "queue"
	SegmentPrefill    SegmentKind = "prefill"
	// SegmentWaiting is the wait for the first token when the engine did not
	// say how much of it was queue and how much prefill.
	SegmentWaiting   SegmentKind = "waiting"
	SegmentThinking  SegmentKind = "thinking"
	SegmentAnswering SegmentKind = "answering"
	// SegmentTail is the time after the last token: the end of the stream.
	SegmentTail SegmentKind = "tail"
)

// SegmentKinds lists every kind in the order a request goes through them.
// The unsplit wait comes before the prefill: an engine that reports only
// its prefill leaves the queue in it, ahead of the prefill.
func SegmentKinds() []SegmentKind {
	return []SegmentKind{SegmentGateway, SegmentPDPrefill, SegmentPreprocess, SegmentQueue, SegmentWaiting, SegmentPrefill, SegmentThinking, SegmentAnswering, SegmentTail}
}

// Segment is one phase's duration in milliseconds.
type Segment struct {
	Kind SegmentKind
	MS   float64
}

// timeline is what both a finished and a live request offer the split.
type timeline struct {
	live                         bool
	endMS                        float64
	dispatchMS                   int32
	pdPrefillMS                  int32
	firstThinkMS, firstAnswerMS  int32
	lastEventMS                  int32
	queueMS, prefillMS, decodeMS int32
}

// SegmentsOf splits a finished request.
func SegmentsOf(r Request) []Segment {
	end := r.EndMS
	if end < 0 {
		end = r.LastChunkMS
	}
	pdPrefill := Unknown
	if r.PD != nil {
		pdPrefill = r.PD.PrefillMS
	}
	return split(timeline{
		endMS: float64(max(end, 0)), dispatchMS: r.DispatchMS, pdPrefillMS: pdPrefill,
		firstThinkMS: r.FirstThinkMS, firstAnswerMS: r.FirstAnswerMS, lastEventMS: r.LastChunkMS,
		queueMS: r.EngineQueueMS, prefillMS: r.EnginePrefillMS, decodeMS: r.EngineDecodeMS,
	})
}

// LiveSegments splits an in-flight request up to its age. A PD request
// still in its prefill stage has not been dispatched: its stage runs to now.
func LiveSegments(l Live) []Segment {
	dispatch, pdPrefill := l.DispatchMS, Unknown
	if l.PDPrefillInstance != "" && known(l.PDPrefillStartMS) {
		if !known(dispatch) {
			dispatch = int32(min(max(l.AgeMS, 0), math.MaxInt32))
		}
		pdPrefill = max(0, dispatch-l.PDPrefillStartMS)
	}
	return split(timeline{
		live: true, endMS: float64(max(l.AgeMS, 0)), dispatchMS: dispatch, pdPrefillMS: pdPrefill,
		firstThinkMS: l.FirstThinkMS, firstAnswerMS: l.FirstAnswerMS, lastEventMS: l.LastEventMS,
		queueMS: Unknown, prefillMS: Unknown, decodeMS: Unknown,
	})
}

func known(v int32) bool { return v >= 0 }

func split(t timeline) []Segment {
	var out []Segment
	push := func(kind SegmentKind, ms float64) {
		if ms > 0 {
			out = append(out, Segment{kind, ms})
		}
	}
	end := t.endMS
	dispatch := 0.0
	if known(t.dispatchMS) {
		dispatch = min(float64(t.dispatchMS), end)
	}
	// The gateway's share up to dispatch, a PD prefill stage taken out of it.
	pushGateway := func() {
		pdPrefill := 0.0
		if known(t.pdPrefillMS) {
			pdPrefill = min(float64(t.pdPrefillMS), dispatch)
		}
		push(SegmentGateway, dispatch-pdPrefill)
		push(SegmentPDPrefill, pdPrefill)
	}
	firstToken, hasFirst := int32(0), false
	for _, v := range []int32{t.firstThinkMS, t.firstAnswerMS} {
		if known(v) && (!hasFirst || v < firstToken) {
			firstToken, hasFirst = v, true
		}
	}
	// A reply that does not stream shows no first token; when the engine
	// accounted for the whole request, its queue, prefill and decode fill the
	// span after dispatch and the rest is the engine's unaccounted share.
	if !t.live && !hasFirst && known(t.queueMS) && known(t.prefillMS) && known(t.decodeMS) {
		span := max(0, end-dispatch)
		engine := float64(t.queueMS) + float64(t.prefillMS) + float64(t.decodeMS)
		scale := 1.0
		if engine > span && engine > 0 {
			scale = span / engine
		}
		pushGateway()
		push(SegmentPreprocess, span-engine*scale)
		push(SegmentQueue, float64(t.queueMS)*scale)
		push(SegmentPrefill, float64(t.prefillMS)*scale)
		push(SegmentAnswering, float64(t.decodeMS)*scale)
		return out
	}
	waitEnd := end
	if hasFirst {
		waitEnd = float64(firstToken)
	}
	wait := max(0, waitEnd-dispatch)
	if known(t.prefillMS) && wait > 0 {
		queue, prefill := 0.0, float64(t.prefillMS)
		if known(t.queueMS) {
			queue = float64(t.queueMS)
		}
		// Engine and gateway clocks measure from different moments; when the
		// engine's account is longer than the wait the gateway saw, keep its
		// proportions inside the wait.
		if queue+prefill > wait {
			scale := wait / (queue + prefill)
			queue *= scale
			prefill *= scale
		}
		rest := wait - queue - prefill
		if known(t.queueMS) {
			// What the engine does not account for is drawn before its
			// queue, where most of it happens.
			pushGateway()
			push(SegmentPreprocess, rest)
			push(SegmentQueue, queue)
		} else {
			// An engine that reports its prefill but not its queue
			// (TensorFold, Strata): the rest of the wait is the queue and the
			// preprocessing together, still one wait.
			pushGateway()
			push(SegmentWaiting, rest)
		}
		push(SegmentPrefill, prefill)
	} else {
		pushGateway()
		push(SegmentWaiting, wait)
	}
	if !hasFirst {
		return out
	}
	lastEvent := end
	if !t.live {
		lastEvent = float64(firstToken)
		if known(t.lastEventMS) {
			lastEvent = max(lastEvent, float64(t.lastEventMS))
		} else {
			lastEvent = max(lastEvent, end)
		}
	}
	if known(t.firstThinkMS) {
		thinkEnd := lastEvent
		if known(t.firstAnswerMS) {
			thinkEnd = float64(max(t.firstAnswerMS, t.firstThinkMS))
		}
		push(SegmentThinking, thinkEnd-float64(t.firstThinkMS))
	}
	if known(t.firstAnswerMS) {
		push(SegmentAnswering, lastEvent-float64(t.firstAnswerMS))
	}
	push(SegmentTail, end-lastEvent)
	return out
}
