package uxstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestSamplerStrideSequence(t *testing.T) {
	sm := sampler{budget: 2}
	normal := okReq("n", 1, 1)
	var got []int32 // 0 = not kept, else the weight
	for range 14 {
		keep, w := sm.keep(&normal, 100)
		if !keep {
			w = 0
		}
		got = append(got, w)
	}
	// Budget 2 at weight 1; stride 2 for two rows; then stride 4 for two
	// rows; then stride 8.
	want := []int32{1, 1, 0, 2, 0, 2, 0, 0, 0, 4, 0, 0, 0, 4}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("weights = %v, want %v", got, want)
	}
	// An anomaly past the budget is kept at weight 1 and does not advance
	// the stride.
	bad := normal
	bad.Status = 503
	if keep, w := sm.keep(&bad, 100); !keep || w != 1 {
		t.Fatalf("anomaly: keep=%v w=%d", keep, w)
	}
	// A new second starts over.
	if keep, w := sm.keep(&normal, 101); !keep || w != 1 {
		t.Fatalf("new second: keep=%v w=%d", keep, w)
	}
}

func TestAnomalousRule(t *testing.T) {
	base := okReq("x", 1, 1)
	cases := []struct {
		name string
		mut  func(r *uxrecord.Request)
		want bool
	}{
		{"fast ok", func(*uxrecord.Request) {}, false},
		{"status 429", func(r *uxrecord.Request) { r.Status = 429 }, true},
		{"status 199", func(r *uxrecord.Request) { r.Status = 199 }, true},
		{"status 204", func(r *uxrecord.Request) { r.Status = 204 }, false},
		{"client closed", func(r *uxrecord.Request) { r.Outcome = uxrecord.OutcomeClientClosed }, true},
		{"truncated", func(r *uxrecord.Request) { r.Outcome = uxrecord.OutcomeTruncated }, true},
		{"length", func(r *uxrecord.Request) { r.FinishReason = "length" }, true},
		{"answer at threshold", func(r *uxrecord.Request) { r.FirstAnswerMS = 10000 }, true},
		{"answer below threshold", func(r *uxrecord.Request) { r.FirstAnswerMS = 9999 }, false},
		{"think first is fast", func(r *uxrecord.Request) { r.FirstThinkMS, r.FirstAnswerMS = 100, 20000 }, false},
		{"no token, slow end", func(r *uxrecord.Request) { r.FirstAnswerMS, r.EndMS = uxrecord.Unknown, 12000 }, true},
		{"no token, fast end", func(r *uxrecord.Request) { r.FirstAnswerMS, r.EndMS = uxrecord.Unknown, 900 }, false},
		{"nothing observed", func(r *uxrecord.Request) { r.FirstAnswerMS, r.EndMS = uxrecord.Unknown, uxrecord.Unknown }, false},
	}
	for _, tc := range cases {
		r := base
		tc.mut(&r)
		if got := anomalous(&r, keepSlowMS); got != tc.want {
			t.Errorf("%s: anomalous = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Under a budget of 10 rows a second, a second with 1010 normal requests and
// five anomalies keeps every anomaly, and the kept normal rows' weights add
// up to the normal traffic within one stride. The grids and coverage still
// count every request.
func TestSamplingBudgetKeepsAnomaliesAndWeights(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 10
	s, _ := openTest(t, opts)
	ts := ms(day0) - 60_000
	const normals = 1010
	for i := range 10 {
		s.Observe(okReq(fmt.Sprintf("n%d", i), 1, ts))
	}
	anomalies := map[string]func(r *uxrecord.Request){
		"a-500":    func(r *uxrecord.Request) { r.Status = 500 },
		"a-closed": func(r *uxrecord.Request) { r.Outcome = uxrecord.OutcomeClientClosed },
		"a-length": func(r *uxrecord.Request) { r.FinishReason = "length" },
		"a-slow":   func(r *uxrecord.Request) { r.FirstAnswerMS = 15000 },
		"a-nonstr": func(r *uxrecord.Request) { r.Stream, r.FirstAnswerMS, r.EndMS = false, uxrecord.Unknown, 11000 },
	}
	for id, mut := range anomalies {
		r := okReq(id, 1, ts)
		mut(&r)
		s.Observe(r)
	}
	for i := 10; i < normals; i++ {
		s.Observe(okReq(fmt.Sprintf("n%d", i), 1, ts))
	}
	mustSync(t, s, false)

	rows := allRequests(t, s, RequestQuery{DeploymentID: 1, Limit: MaxRequestLimit})
	var normalWeight, normalRows int64
	var maxWeight int32
	seen := map[string]bool{}
	for _, r := range rows {
		if _, ok := anomalies[r.ID]; ok {
			seen[r.ID] = true
			if r.Weight != 1 {
				t.Errorf("anomaly %s weight %d", r.ID, r.Weight)
			}
			continue
		}
		normalRows++
		normalWeight += int64(r.Weight)
		maxWeight = max(maxWeight, r.Weight)
	}
	if len(seen) != len(anomalies) {
		t.Fatalf("anomalies kept %v, want all %d", seen, len(anomalies))
	}
	if normalWeight > normals || normalWeight <= normals-int64(maxWeight) {
		t.Fatalf("normal weights sum %d, want within one stride (%d) of %d", normalWeight, maxWeight, normals)
	}
	if normalRows >= 100 {
		t.Fatalf("kept %d normal rows of %d: sampling did not engage", normalRows, normals)
	}
	st := s.Stats()
	if st.SampledAway != uint64(normals-normalRows) || st.RowsWritten != uint64(len(rows)) {
		t.Fatalf("stats %+v, rows %d normal rows %d", st, len(rows), normalRows)
	}
	cov, err := s.Coverage(context.Background(), 1, 0, 0)
	if err != nil || cov.Total != normals+int64(len(anomalies)) {
		t.Fatalf("coverage = %+v, %v", cov, err)
	}
	cells, err := s.Grid(context.Background(), GridQuery{DeploymentID: 1, Kind: uxrecord.GridIO})
	if err != nil {
		t.Fatal(err)
	}
	var gridN int64
	for _, cell := range cells {
		gridN += cell.N
	}
	if gridN != cov.WithUsage || gridN != normals+int64(len(anomalies)) {
		t.Fatalf("io grid counts %d, want every request (%d)", gridN, cov.WithUsage)
	}

	// The next second has a fresh budget.
	c.Set(day0.Add(time.Second))
	for i := range 10 {
		s.Observe(okReq(fmt.Sprintf("m%d", i), 2, ts))
	}
	mustSync(t, s, false)
	for _, r := range allRequests(t, s, RequestQuery{DeploymentID: 2}) {
		if r.Weight != 1 {
			t.Fatalf("new second row %s weight %d", r.ID, r.Weight)
		}
	}
}
