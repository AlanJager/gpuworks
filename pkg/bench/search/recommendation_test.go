package search

import "testing"

func TestBestMeasuredUsesSharedThresholdsAndObjective(t *testing.T) {
	complete, incomplete := 1.0, .75
	points := []Observation{
		{Point: Point{Concurrency: 1}, Throughput: 100, P99TTFT: 10, SessionCompletion: &incomplete},
		{Point: Point{Concurrency: 2}, Throughput: 90, P99TTFT: 20, SessionCompletion: &complete},
		{Point: Point{Concurrency: 4}, Throughput: 110, P99TTFT: 80, SessionCompletion: &complete},
	}
	best, ok := BestMeasured(points, SLAConfig{MaxP99TTFT: 50, MinSessionCompletion: .99})
	if !ok || best.Point.Concurrency != 2 {
		t.Fatalf("best=%+v ok=%v", best, ok)
	}
	if _, ok := BestMeasured(points, SLAConfig{MaxP99TTFT: 5}); ok {
		t.Fatal("must not recommend without a qualifying measured point")
	}
	// No hidden 99% request-success default or 90%-of-peak heuristic.
	points[0].SuccessRate = .5
	best, ok = BestMeasured(points[:2], SLAConfig{})
	if !ok || best.Point.Concurrency != 1 {
		t.Fatalf("undeclared threshold changed selection: %+v", best)
	}
}

func TestTimingThresholdRequiresEvidenceAndPreservesLegacyObservations(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation Observation
		want        bool
	}{
		{"legacy nonzero", Observation{P99TTFT: 5}, true},
		{"legacy unknown zero", Observation{}, false},
		{"explicit no samples", Observation{P99TTFT: 5, Samples: &SampleCounts{}}, false},
		{"observed zero", Observation{Samples: &SampleCounts{TTFT: 1}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			violations := (SLAConfig{MaxP99TTFT: 10}).Violations(tc.observation)
			if (len(violations) == 0) != tc.want {
				t.Fatalf("violations=%+v", violations)
			}
			if !tc.want && violations[0].Reason != "evidence_unavailable" {
				t.Fatalf("missing evidence mistaken for a measured violation: %+v", violations)
			}
		})
	}
	for _, constraint := range []SLAConfig{{MaxP99TPOT: 10}, {MaxP99Latency: 10}, {MinInteractivity: 10}, {MinSessionCompletion: .5}} {
		if _, ok := BestMeasured([]Observation{{}}, constraint); ok {
			t.Fatalf("unknown timing/session data passed %+v", constraint)
		}
	}
}

func TestBestMeasuredObjectiveTieAndDocument(t *testing.T) {
	points := []Observation{{Point: Point{Concurrency: 4}, RequestTput: 8}, {Point: Point{Concurrency: 2}, RequestTput: 8}}
	best, ok := BestMeasured(points, SLAConfig{Objective: "rps"})
	if !ok || best.Point.Concurrency != 2 {
		t.Fatalf("tie=%+v ok=%v", best, ok)
	}
	doc, err := ParseDocument([]byte(`{"min_session_completion":0.95}`))
	if err != nil || doc.SLA().MinSessionCompletion != .95 {
		t.Fatalf("document=%+v err=%v", doc, err)
	}
}

func TestWithEvidenceKeepsUnknownVersionAndReadsSampleCounts(t *testing.T) {
	for _, raw := range []string{`{}`, `{"Evidence":{"version":2,"plan":{"planned_sessions":4}}}`} {
		got := WithEvidence(Observation{P99TTFT: 7}, []byte(raw))
		if got.Samples != nil || got.SessionCompletion != nil || got.P99TTFT != 7 {
			t.Fatalf("unknown document changed old observation: %+v", got)
		}
	}
	got := WithEvidence(Observation{}, []byte(`{"Evidence":{"version":1,"plan":{"planned_sessions":4},"outcomes":{"completed_sessions":3},"all":{"ttft":{"count":40},"tpot":{"count":36}}}}`))
	if got.Samples == nil || got.Samples.TTFT != 40 || got.Samples.TPOT != 36 || got.SessionCompletion == nil || *got.SessionCompletion != .75 {
		t.Fatalf("evidence=%+v", got)
	}
}

func TestBestMeasuredNeedsPositiveCompletedWork(t *testing.T) {
	for _, points := range [][]Observation{nil, {{Throughput: 0}}, {{Throughput: -1}}} {
		if _, ok := BestMeasured(points, SLAConfig{}); ok {
			t.Fatal("empty or zero-work set produced recommendation")
		}
	}
}
