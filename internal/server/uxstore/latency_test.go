package uxstore

import (
	"context"
	"database/sql"
	"math"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Every whole reply adds to the latency sums and the segment histograms,
// sampled out of the log or not; a reply the client left adds nothing.
func TestLatencySumsCountEveryWholeReply(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 1 // the log keeps one row a second; the sums keep all
	s, _ := openTest(t, opts)
	for i := 0; i < 10; i++ {
		s.Observe(okReq("ok", 1, ms(day0)+int64(i)))
	}
	left := okReq("left", 1, ms(day0)+20)
	left.Outcome = uxrecord.OutcomeClientClosed
	s.Observe(left)
	s.Observe(okReq("other", 2, ms(day0)+30))
	mustSync(t, s, false)

	sums, err := s.Latency(context.Background(), 1, ms(day0)-1, ms(day0)+60_000)
	if err != nil {
		t.Fatal(err)
	}
	// okReq: dispatch 1, first answer 80, last chunk 900, end 901.
	if sums.Requests != 10 || sums.EndToEndMS != 9010 {
		t.Fatalf("requests %d e2e %v, want 10 and 9010", sums.Requests, sums.EndToEndMS)
	}
	want := map[uxrecord.SegmentKind]float64{
		uxrecord.SegmentGateway: 10, uxrecord.SegmentWaiting: 790, uxrecord.SegmentAnswering: 8200, uxrecord.SegmentTail: 10,
	}
	for kind, ms := range want {
		if math.Abs(sums.SegmentMS[kind]-ms) > 1e-6 {
			t.Errorf("%s = %v, want %v", kind, sums.SegmentMS[kind], ms)
		}
	}
	// 100 output tokens over 820 ms from the first token, ten times.
	if sums.DecodeTokens != 990 || sums.DecodeMS != 8200 {
		t.Errorf("decode = %v tokens / %v ms", sums.DecodeTokens, sums.DecodeMS)
	}
	cells, err := s.Grid(context.Background(), GridQuery{DeploymentID: 1, Kind: uxrecord.GridSegments, FromMS: ms(day0) - 1, ToMS: ms(day0) + 60_000})
	if err != nil {
		t.Fatal(err)
	}
	e2e := uxrecord.LatencyRow(uxrecord.LatencyEndToEnd)
	var n int64
	for _, cell := range cells {
		if cell.X == e2e {
			n += cell.N
			if cell.Y != uxrecord.LatencyBin(901) {
				t.Errorf("e2e cell in bin %d, want %d", cell.Y, uxrecord.LatencyBin(901))
			}
		}
	}
	if n != 10 {
		t.Fatalf("e2e histogram counts %d, want 10", n)
	}
	if _, err := s.Latency(context.Background(), 0, 0, 0); err == nil {
		t.Fatal("a latency query without a deployment was accepted")
	}
}

// A day written before the latency table existed gets it when the store
// opens, so the first flush after an upgrade does not fail on it.
func TestOpenCompletesADayWrittenBeforeTheLatencyTable(t *testing.T) {
	c := newClock(day0)
	s, path := openTest(t, testOpts(c))
	s.Observe(okReq("before", 1, ms(day0)))
	mustSync(t, s, false)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Take the day back to its shape before the upgrade.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE ` + latPrefix + dayOf(ms(day0))); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s = openAt(t, path, testOpts(c))
	s.Observe(okReq("after", 1, ms(day0)+1000))
	mustSync(t, s, false)
	sums, err := s.Latency(context.Background(), 1, ms(day0)-1, ms(day0)+60_000)
	if err != nil {
		t.Fatal(err)
	}
	if sums.Requests != 1 {
		t.Fatalf("requests after the upgrade = %d, want the one written since", sums.Requests)
	}
	if got := len(allRequests(t, s, RequestQuery{DeploymentID: 1})); got != 2 {
		t.Fatalf("request log holds %d rows, want both", got)
	}
}

// engineReq is okReq with vLLM's per-request stats: the gateway dispatches
// at 3 ms, the first token arrives at 500 ms and the engine accounts for 100
// ms of queue and 200 of prefill, which leaves 197 ms of preprocessing.
func engineReq(id string, ts int64) uxrecord.Request {
	r := okReq(id, 1, ts)
	r.DispatchMS, r.FirstAnswerMS, r.EngineQueueMS, r.EnginePrefillMS = 3, 500, 100, 200
	return r
}

// The gateway's own share and the engine's preprocessing are summed apart
// and have a histogram row each.
func TestLatencyTellsTheGatewayFromThePreprocessing(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	s.Observe(engineReq("a", ms(day0)))
	s.Observe(engineReq("b", ms(day0)+1))
	mustSync(t, s, false)
	sums, err := s.Latency(context.Background(), 1, ms(day0)-1, ms(day0)+60_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := sums.SegmentMS; got[uxrecord.SegmentGateway] != 6 || got[uxrecord.SegmentPreprocess] != 394 || got[uxrecord.SegmentQueue] != 200 {
		t.Fatalf("segments = %v, want gateway 6, preprocessing 394, queue 200", got)
	}
	cells, err := s.Grid(context.Background(), GridQuery{DeploymentID: 1, Kind: uxrecord.GridSegments, FromMS: ms(day0) - 1, ToMS: ms(day0) + 60_000})
	if err != nil {
		t.Fatal(err)
	}
	bins := map[int]int{}
	for _, cell := range cells {
		if cell.X == uxrecord.LatencyRow("gateway") || cell.X == uxrecord.LatencyRow("preprocess") {
			bins[cell.X] = cell.Y
		}
	}
	if bins[uxrecord.LatencyRow("gateway")] != uxrecord.LatencyBin(3) || bins[uxrecord.LatencyRow("preprocess")] != uxrecord.LatencyBin(197) {
		t.Fatalf("histogram bins = %v", bins)
	}
}
