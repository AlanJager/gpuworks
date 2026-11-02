package uxrecord

import (
	"reflect"
	"testing"
)

// done is a finished request with nothing observed unless a case says so.
func done(over func(r *Request)) Request {
	r := Request{
		DispatchMS: Unknown, FirstByteMS: Unknown, FirstThinkMS: Unknown, FirstAnswerMS: Unknown,
		LastChunkMS: Unknown, EndMS: Unknown, EngineQueueMS: Unknown, EnginePrefillMS: Unknown, EngineDecodeMS: Unknown,
		Status: 200,
	}
	over(&r)
	return r
}

func TestSegmentsOfAFinishedRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Request
		want []Segment
	}{
		{
			"waits, thinks and answers without engine stats",
			done(func(r *Request) {
				r.DispatchMS, r.FirstThinkMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 2, 300, 1300, 3300, 3310
			}),
			[]Segment{{SegmentGateway, 2}, {SegmentWaiting, 298}, {SegmentThinking, 1000}, {SegmentAnswering, 2000}, {SegmentTail, 10}},
		},
		{
			"the engine's queue and prefill split the wait; the rest is the engine's preprocessing, after the gateway's own share",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.EngineQueueMS, r.EnginePrefillMS = 2, 502, 1502, 1502, 100, 350
			}),
			[]Segment{{SegmentGateway, 2}, {SegmentPreprocess, 50}, {SegmentQueue, 100}, {SegmentPrefill, 350}, {SegmentAnswering, 1000}},
		},
		{
			"a prefill without a queue splits the prefill off and leaves the rest one wait",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.EnginePrefillMS = 1, 99, 299, 301, 56
			}),
			[]Segment{{SegmentGateway, 1}, {SegmentWaiting, 42}, {SegmentPrefill, 56}, {SegmentAnswering, 200}, {SegmentTail, 2}},
		},
		{
			"a reported queue without prefill remains queue plus an unsplit wait",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.EngineQueueMS = 2, 20302, 21302, 21302, 20000
			}),
			[]Segment{{SegmentGateway, 2}, {SegmentQueue, 20000}, {SegmentWaiting, 300}, {SegmentAnswering, 1000}},
		},
		{
			"a queue-only account cannot exceed the observed wait",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.EngineQueueMS = 2, 302, 1302, 1302, 20000
			}),
			[]Segment{{SegmentGateway, 2}, {SegmentQueue, 300}, {SegmentAnswering, 1000}},
		},
		{
			"an engine account longer than the gateway's wait keeps its proportions inside the wait",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.EngineQueueMS, r.EnginePrefillMS = 0, 100, 200, 200, 100, 100
			}),
			[]Segment{{SegmentQueue, 50}, {SegmentPrefill, 50}, {SegmentAnswering, 100}},
		},
		{
			"a reply without tokens is waiting up to its end",
			done(func(r *Request) { r.DispatchMS, r.EndMS = 0, 800 }),
			[]Segment{{SegmentWaiting, 800}},
		},
		{
			"a reply that does not stream is split by the engine's queue, prefill and decode",
			done(func(r *Request) {
				r.DispatchMS, r.EndMS, r.EngineQueueMS, r.EnginePrefillMS, r.EngineDecodeMS = 2, 1002, 100, 200, 600
			}),
			[]Segment{{SegmentGateway, 2}, {SegmentPreprocess, 100}, {SegmentQueue, 100}, {SegmentPrefill, 200}, {SegmentAnswering, 600}},
		},
		{
			"without a decode time the engine splits only the wait",
			done(func(r *Request) { r.DispatchMS, r.EndMS, r.EngineQueueMS, r.EnginePrefillMS = 0, 500, 10, 20 }),
			[]Segment{{SegmentPreprocess, 470}, {SegmentQueue, 10}, {SegmentPrefill, 20}},
		},
		{
			"an unknown end falls back to the last chunk",
			done(func(r *Request) { r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS = 0, 100, 400 }),
			[]Segment{{SegmentWaiting, 100}, {SegmentAnswering, 300}},
		},
	} {
		if got := SegmentsOf(tc.r); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %v\nwant %v", tc.name, got, tc.want)
		}
	}
}

func TestLiveSegmentsRunToNow(t *testing.T) {
	waiting := LiveSegments(Live{AgeMS: 4000, DispatchMS: 1, FirstThinkMS: Unknown, FirstAnswerMS: Unknown, LastEventMS: Unknown})
	if want := []Segment{{SegmentGateway, 1}, {SegmentWaiting, 3999}}; !reflect.DeepEqual(waiting, want) {
		t.Errorf("waiting: %v, want %v", waiting, want)
	}
	thinking := LiveSegments(Live{AgeMS: 5000, DispatchMS: 0, FirstThinkMS: 1000, FirstAnswerMS: Unknown, LastEventMS: 4900})
	if want := []Segment{{SegmentWaiting, 1000}, {SegmentThinking, 4000}}; !reflect.DeepEqual(thinking, want) {
		t.Errorf("thinking: %v, want %v", thinking, want)
	}
}

// However a request is split, its segments add up to its end-to-end time:
// what lets the breakdown average segments.
func TestSegmentsAddUpToTheEndToEndTime(t *testing.T) {
	for _, r := range []Request{
		done(func(r *Request) {
			r.DispatchMS, r.FirstThinkMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 3, 40, 900, 5000, 5100
		}),
		done(func(r *Request) {
			r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.EngineQueueMS, r.EnginePrefillMS = 1, 300, 800, 805, 700, 500
		}),
		done(func(r *Request) {
			r.DispatchMS, r.EndMS, r.EngineQueueMS, r.EnginePrefillMS, r.EngineDecodeMS = 5, 700, 400, 400, 400
		}),
		done(func(r *Request) { r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 9, 20, 15, 30 }),
	} {
		sum := 0.0
		for _, s := range SegmentsOf(r) {
			sum += s.MS
		}
		if diff := sum - float64(r.EndMS); diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%+v: segments add up to %v, end is %d", r, sum, r.EndMS)
		}
	}
}

// A PD request's prefill stage is taken out of the gateway's share and
// drawn right after it; the segments still add up to the end to end. In PD
// the decode instance's queue holds the wait for the KV and its prefill the
// recompute of the last token, drawn as for any engine.
func TestSegmentsOfAPDRequest(t *testing.T) {
	pd := func(prefill int32) *PDStage {
		return &PDStage{PrefillInstance: "p0", PrefillMS: prefill, PrefillQueueMS: 1, PrefillComputeMS: 700}
	}
	for _, tc := range []struct {
		name string
		r    Request
		want []Segment
	}{
		{
			"sequential handoff: 21 ms of gateway, 795 ms of prefill stage before dispatch",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 816, 2080, 3080, 3081
				r.EngineQueueMS, r.EnginePrefillMS = 1200, 29
				r.PD = pd(795)
			}),
			[]Segment{{SegmentGateway, 21}, {SegmentPDPrefill, 795}, {SegmentPreprocess, 35}, {SegmentQueue, 1200}, {SegmentPrefill, 29}, {SegmentAnswering, 1000}, {SegmentTail, 1}},
		},
		{
			"a stage reported longer than the dispatch (clock skew) is capped at it",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 100, 200, 300, 300
				r.PD = pd(150)
			}),
			[]Segment{{SegmentPDPrefill, 100}, {SegmentWaiting, 100}, {SegmentAnswering, 100}},
		},
		{
			"concurrent handoff: the gateway did not wait, no stage",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 3, 200, 300, 300
				r.PD = &PDStage{PrefillInstance: "p0", PrefillMS: Unknown, PrefillQueueMS: Unknown, PrefillComputeMS: Unknown}
			}),
			[]Segment{{SegmentGateway, 3}, {SegmentWaiting, 197}, {SegmentAnswering, 100}},
		},
		{
			"a fallback that tried a prefill instance first: its stage is still the wait",
			done(func(r *Request) {
				r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 5005, 5100, 5200, 5200
				r.PD = &PDStage{PrefillMS: 5000, PrefillQueueMS: Unknown, PrefillComputeMS: Unknown, Fallback: "prefill_unreachable"}
			}),
			[]Segment{{SegmentGateway, 5}, {SegmentPDPrefill, 5000}, {SegmentWaiting, 95}, {SegmentAnswering, 100}},
		},
		{
			"a non-streamed PD reply with the engine's full account",
			done(func(r *Request) {
				r.DispatchMS, r.EndMS = 500, 1500
				r.EngineQueueMS, r.EnginePrefillMS, r.EngineDecodeMS = 300, 20, 600
				r.PD = pd(480)
			}),
			[]Segment{{SegmentGateway, 20}, {SegmentPDPrefill, 480}, {SegmentPreprocess, 80}, {SegmentQueue, 300}, {SegmentPrefill, 20}, {SegmentAnswering, 600}},
		},
	} {
		got := SegmentsOf(tc.r)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %v\nwant %v", tc.name, got, tc.want)
		}
		sum := 0.0
		for _, s := range got {
			sum += s.MS
		}
		if sum != float64(tc.r.EndMS) {
			t.Errorf("%s: segments add up to %v, want %d", tc.name, sum, tc.r.EndMS)
		}
	}
}

// A PD request in flight: while in its prefill stage it has no dispatch yet
// and the stage runs to now; after it, the stage is fixed and the wait for
// the first token follows.
func TestLiveSegmentsOfAPDRequest(t *testing.T) {
	in := Live{AgeMS: 900, DispatchMS: Unknown, FirstThinkMS: Unknown, FirstAnswerMS: Unknown, LastEventMS: Unknown,
		PDPrefillInstance: "p0", PDPrefillStartMS: 20}
	if got, want := LiveSegments(in), []Segment{{SegmentGateway, 20}, {SegmentPDPrefill, 880}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("in the stage: %v, want %v", got, want)
	}
	after := in
	after.AgeMS, after.DispatchMS = 1500, 1000
	if got, want := LiveSegments(after), []Segment{{SegmentGateway, 20}, {SegmentPDPrefill, 980}, {SegmentWaiting, 500}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the stage: %v, want %v", got, want)
	}
}

// The prefill stage has its own latency row, appended so stored rows keep
// their places; a PD reply's breakdown sums it.
func TestLatencyOfAPDRequestHasItsStage(t *testing.T) {
	if LatencyRow(string(SegmentPDPrefill)) != len(LatencyRows())-1 || LatencyRow(string(SegmentPreprocess)) != 8 {
		t.Fatalf("rows = %v", LatencyRows())
	}
	r := done(func(r *Request) {
		r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 816, 2080, 3080, 3081
		r.PD = &PDStage{PrefillInstance: "p0", PrefillMS: 795, PrefillQueueMS: Unknown, PrefillComputeMS: Unknown}
	})
	sums, cells, ok := LatencyOf(r)
	if !ok || sums.SegmentMS[SegmentPDPrefill] != 795 || sums.SegmentMS[SegmentGateway] != 21 {
		t.Fatalf("sums = %+v", sums)
	}
	found := false
	for _, c := range cells {
		found = found || c.X == LatencyRow(string(SegmentPDPrefill))
	}
	if !found {
		t.Fatal("no histogram cell for the stage")
	}
}
