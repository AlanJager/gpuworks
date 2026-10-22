package uxrecord

import "math"

// The latency breakdown: what a deployment's whole replies spent in each
// segment (SegmentsOf), summed so the means add up to the mean end-to-end
// time, plus the token and time sums its speeds are the ratio of, and a
// histogram per segment for its percentiles. Only whole replies count
// (ResultCompleted and ResultLength): a reply the client left or the
// connection cut has no end-to-end time worth averaging.

// GridSegments is the grid kind of the per-segment histograms: x is the
// segment (LatencyRow), y the latency bin (LatencyBin).
const GridSegments = "seg"

// LatencyRows are the histogram rows, a row's index being its x in the
// grid. The order is stored, so it never changes: a new row is appended.
// Before the gateway's own share was told apart from the engine's
// preprocessing, row 0 held both; the store's schema version 2 moved those
// cells to the preprocessing row.
func LatencyRows() []string {
	return []string{
		string(SegmentGateway), string(SegmentQueue), string(SegmentPrefill), string(SegmentWaiting),
		string(SegmentThinking), string(SegmentAnswering), string(SegmentTail), LatencyEndToEnd,
		string(SegmentPreprocess), string(SegmentPDPrefill),
	}
}

// LatencyEndToEnd names the end-to-end row of the histograms.
const LatencyEndToEnd = "e2e"

// LatencyRow is a histogram row's index in LatencyRows.
func LatencyRow(name string) int {
	for i, row := range LatencyRows() {
		if row == name {
			return i
		}
	}
	return -1
}

// latencyBinsPerOctave makes bins a quarter octave wide: a percentile read
// back from them is within about 10% of the true value.
const latencyBinsPerOctave = 4

// LatencyBin is the histogram bin of a duration in milliseconds; bin 0 holds
// everything up to 1 ms.
func LatencyBin(ms float64) int {
	if ms <= 1 {
		return 0
	}
	return int(math.Floor(math.Log2(ms)*latencyBinsPerOctave)) + 1
}

// LatencyBinMS is a bin's representative duration: the geometric middle of
// its range.
func LatencyBinMS(bin int) float64 {
	if bin <= 0 {
		return 1
	}
	return math.Exp2((float64(bin-1) + 0.5) / latencyBinsPerOctave)
}

// LatencySums is one bucket's sums for one deployment.
type LatencySums struct {
	// Requests counts the whole replies; EndToEndMS and SegmentMS sum their
	// times (a reply without a segment adds 0 to it).
	Requests   int64
	EndToEndMS float64
	SegmentMS  map[SegmentKind]float64
	// Speeds: the tokens and milliseconds of the replies that gave both.
	PrefillTokens, PrefillMS     float64
	ThinkingTokens, ThinkingMS   float64
	AnsweringTokens, AnsweringMS float64
	DecodeTokens, DecodeMS       float64
}

// Merge adds other into s.
func (s *LatencySums) Merge(other LatencySums) {
	s.Requests += other.Requests
	s.EndToEndMS += other.EndToEndMS
	for kind, ms := range other.SegmentMS {
		if s.SegmentMS == nil {
			s.SegmentMS = map[SegmentKind]float64{}
		}
		s.SegmentMS[kind] += ms
	}
	s.PrefillTokens += other.PrefillTokens
	s.PrefillMS += other.PrefillMS
	s.ThinkingTokens += other.ThinkingTokens
	s.ThinkingMS += other.ThinkingMS
	s.AnsweringTokens += other.AnsweringTokens
	s.AnsweringMS += other.AnsweringMS
	s.DecodeTokens += other.DecodeTokens
	s.DecodeMS += other.DecodeMS
}

// LatencyOf is one request's contribution: its sums and the histogram cells
// (x = LatencyRow, y = LatencyBin) it adds to. ok is false for a request
// that is not a whole reply.
func LatencyOf(r Request) (sums LatencySums, cells []Cell, ok bool) {
	if result := ResultOf(r); result != ResultCompleted && result != ResultLength {
		return LatencySums{}, nil, false
	}
	segments := SegmentsOf(r)
	end := 0.0
	sums.SegmentMS = make(map[SegmentKind]float64, len(segments))
	for _, s := range segments {
		sums.SegmentMS[s.Kind] += s.MS
		end += s.MS
	}
	if end <= 0 {
		return LatencySums{}, nil, false
	}
	sums.Requests = 1
	sums.EndToEndMS = end
	for kind, ms := range sums.SegmentMS {
		cells = append(cells, Cell{X: LatencyRow(string(kind)), Y: LatencyBin(ms), N: 1})
	}
	cells = append(cells, Cell{X: LatencyRow(LatencyEndToEnd), Y: LatencyBin(end), N: 1})

	if !r.Usage {
		return sums, cells, true
	}
	// Prefill computes the prompt the cache did not cover, in the engine's
	// own prefill time.
	if uncached, ms := r.UncachedTokens(), r.UncachedComputeMS(); uncached > 0 && ms > 0 {
		sums.PrefillTokens, sums.PrefillMS = float64(uncached), float64(ms)
	}
	thinking, answering := sums.SegmentMS[SegmentThinking], sums.SegmentMS[SegmentAnswering]
	reasoningKnown := r.ReasoningTokens >= 0
	if reasoningKnown && r.ReasoningTokens > 0 && thinking > 0 {
		sums.ThinkingTokens, sums.ThinkingMS = float64(r.ReasoningTokens), thinking
	}
	// The answer's tokens are the output less the reasoning; without a
	// reasoning count they are known only when the reply did not think.
	if answering > 0 && r.CompletionTokens > 0 {
		switch {
		case reasoningKnown && r.CompletionTokens > r.ReasoningTokens:
			sums.AnsweringTokens, sums.AnsweringMS = float64(r.CompletionTokens-r.ReasoningTokens), answering
		case !reasoningKnown && thinking == 0:
			sums.AnsweringTokens, sums.AnsweringMS = float64(r.CompletionTokens), answering
		}
	}
	// The whole decode: the engine's decode time when it gave one, else the
	// stream from the first token to the last, which the tokens after the
	// first took.
	switch {
	case r.CompletionTokens > 1 && r.EngineDecodeMS > 0:
		sums.DecodeTokens, sums.DecodeMS = float64(r.CompletionTokens), float64(r.EngineDecodeMS)
	case r.CompletionTokens > 1 && r.FirstTokenMS() >= 0 && r.LastChunkMS > r.FirstTokenMS():
		sums.DecodeTokens, sums.DecodeMS = float64(r.CompletionTokens-1), float64(r.LastChunkMS-r.FirstTokenMS())
	}
	return sums, cells, true
}
