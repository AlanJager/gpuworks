package uxrecord

import (
	"math"
	"testing"
)

func TestLatencyBinsAreQuarterOctaves(t *testing.T) {
	for _, tc := range []struct {
		ms   float64
		want int
	}{{0, 0}, {1, 0}, {1.1, 1}, {2, 5}, {1000, 40}, {60_000, 64}} {
		if got := LatencyBin(tc.ms); got != tc.want {
			t.Errorf("LatencyBin(%v) = %d, want %d", tc.ms, got, tc.want)
		}
	}
	// A bin's representative duration lands back in the bin, within 10% of
	// any duration the bin holds.
	for _, ms := range []float64{3, 47, 999, 12_345, 600_000} {
		mid := LatencyBinMS(LatencyBin(ms))
		if LatencyBin(mid) != LatencyBin(ms) || math.Abs(mid-ms)/ms > 0.1 {
			t.Errorf("%v ms: bin middle %v", ms, mid)
		}
	}
}

// A whole reply contributes its segments, its end-to-end time and the
// speeds its figures allow; the segments' sums add up to the end-to-end sum.
func TestLatencyOfAWholeReply(t *testing.T) {
	r := done(func(r *Request) {
		r.DispatchMS, r.FirstThinkMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 2, 300, 1300, 3300, 3310
		r.EngineQueueMS, r.EnginePrefillMS = 100, 150
		r.Usage, r.PromptTokens, r.CachedTokens, r.CompletionTokens, r.ReasoningTokens = true, 3000, 1000, 300, 100
		r.FinishReason = "stop"
	})
	sums, cells, ok := LatencyOf(r)
	if !ok || sums.Requests != 1 || sums.EndToEndMS != 3310 {
		t.Fatalf("sums = %+v ok %v", sums, ok)
	}
	total := 0.0
	for _, ms := range sums.SegmentMS {
		total += ms
	}
	if total != sums.EndToEndMS {
		t.Fatalf("segments add up to %v, end-to-end %v", total, sums.EndToEndMS)
	}
	if sums.SegmentMS[SegmentQueue] != 100 || sums.SegmentMS[SegmentPrefill] != 150 || sums.SegmentMS[SegmentGateway] != 2 || sums.SegmentMS[SegmentPreprocess] != 48 {
		t.Fatalf("wait split = %v", sums.SegmentMS)
	}
	if sums.PrefillTokens != 2000 || sums.PrefillMS != 150 {
		t.Errorf("prefill speed inputs = %v / %v ms", sums.PrefillTokens, sums.PrefillMS)
	}
	if sums.ThinkingTokens != 100 || sums.ThinkingMS != 1000 || sums.AnsweringTokens != 200 || sums.AnsweringMS != 2000 {
		t.Errorf("thinking %v/%v answering %v/%v", sums.ThinkingTokens, sums.ThinkingMS, sums.AnsweringTokens, sums.AnsweringMS)
	}
	if sums.DecodeTokens != 299 || sums.DecodeMS != 3000 {
		t.Errorf("decode from the stream = %v / %v ms", sums.DecodeTokens, sums.DecodeMS)
	}
	rows := map[int]bool{}
	for _, c := range cells {
		rows[c.X] = true
	}
	for _, name := range []string{"gateway", "preprocess", "queue", "prefill", "thinking", "answering", "tail", LatencyEndToEnd} {
		if !rows[LatencyRow(name)] {
			t.Errorf("no histogram cell for %s", name)
		}
	}
	if rows[LatencyRow("waiting")] {
		t.Error("a split wait has no waiting cell")
	}
}

func TestLatencyOfSkipsWhatIsNotAWholeReplyAndUnknownSpeeds(t *testing.T) {
	for name, r := range map[string]Request{
		"client left": done(func(r *Request) { r.EndMS, r.Outcome = 500, OutcomeClientClosed }),
		"error":       done(func(r *Request) { r.EndMS, r.Status = 500, 503 }),
		"no time":     done(func(*Request) {}),
	} {
		if _, _, ok := LatencyOf(r); ok {
			t.Errorf("%s counted", name)
		}
	}
	// At the length limit it is whole. Without a reasoning count the answer
	// speed of a reply that thought is unknown; the engine's decode time
	// is preferred for the whole decode.
	r := done(func(r *Request) {
		r.DispatchMS, r.FirstThinkMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 0, 100, 600, 1600, 1600
		r.Usage, r.PromptTokens, r.CachedTokens, r.CompletionTokens, r.ReasoningTokens = true, 10, Unknown, 50, Unknown
		r.EngineDecodeMS = 1400
		r.FinishReason = "length"
	})
	sums, _, ok := LatencyOf(r)
	if !ok || sums.AnsweringMS != 0 || sums.ThinkingMS != 0 || sums.PrefillMS != 0 || sums.DecodeTokens != 50 || sums.DecodeMS != 1400 {
		t.Fatalf("sums = %+v ok %v", sums, ok)
	}
}

// The histogram rows are stored by index, so their order is fixed whatever
// order the segments are shown in.
func TestLatencyRowsKeepTheirStoredIndexes(t *testing.T) {
	want := []string{"gateway", "queue", "prefill", "waiting", "thinking", "answering", "tail", "e2e", "preprocess"}
	for i, name := range want {
		if LatencyRow(name) != i {
			t.Errorf("%s is row %d, stored as %d", name, LatencyRow(name), i)
		}
	}
	for _, kind := range SegmentKinds() {
		if LatencyRow(string(kind)) < 0 {
			t.Errorf("segment %s has no histogram row", kind)
		}
	}
}

// A PD handoff's prompt is computed by the prefill instance: the prefill
// speed is its uncached prompt over the prefill instance's own prefill, not
// the decode instance's recompute of the last token. A request the decoder
// computed alone, or a stage without the prefill instance's account,
// falls back to the decoder's own figures or none.
func TestLatencyOfAPDReplyTakesThePrefillInstancesCompute(t *testing.T) {
	pd := func(stage PDStage) Request {
		return done(func(r *Request) {
			r.DispatchMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS = 800, 1200, 2200, 2210
			r.EngineQueueMS, r.EnginePrefillMS = 300, 25
			r.Usage, r.PromptTokens, r.CachedTokens, r.CompletionTokens = true, 1036, 512, 64
			r.FinishReason = "stop"
			r.PD = &stage
		})
	}
	for _, c := range []struct {
		name       string
		stage      PDStage
		tokens, ms float64
	}{
		{"handoff", PDStage{PrefillInstance: "p", PrefillMS: 790, PrefillQueueMS: 2, PrefillComputeMS: 640}, 524, 640},
		{"fallback", PDStage{PrefillMS: 3, PrefillQueueMS: Unknown, PrefillComputeMS: Unknown, Fallback: "prefill_5xx"}, 524, 25},
		{"no account", PDStage{PrefillInstance: "p", PrefillMS: 790, PrefillQueueMS: Unknown, PrefillComputeMS: Unknown}, 0, 0},
	} {
		sums, _, ok := LatencyOf(pd(c.stage))
		if !ok || sums.PrefillTokens != c.tokens || sums.PrefillMS != c.ms {
			t.Errorf("%s: prefill speed inputs = %v / %v ms", c.name, sums.PrefillTokens, sums.PrefillMS)
		}
	}
}
