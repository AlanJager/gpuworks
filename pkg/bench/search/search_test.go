package search

import "testing"

func TestGridStrategy(t *testing.T) {
	s := New(Config{Mode: Grid, Concurrencies: []int{1, 2}, ISLs: []int{8}, OSLs: []int{4, 8}})
	count := 0
	for {
		if _, ok := s.Next(); !ok {
			break
		}
		count++
	}
	if count != 4 {
		t.Fatalf("grid points = %d", count)
	}
}

func TestRequestsForPointUsesConcurrencyMultiplier(t *testing.T) {
	if got, err := mustDoc(t, `{"requests_per_point_multiplier":10,"sessions_per_point":{"3":7}}`).RequestsForPoint(Point{Concurrency: 3}); err != nil || got != 7 {
		t.Fatalf("point-specific session count = %d, want 7", got)
	}
	if got, err := mustDoc(t, `{"requests_per_point_multiplier":10}`).RequestsForPoint(Point{Concurrency: 3}); err != nil || got != 30 {
		t.Fatalf("default multiplier result = %d, want 30", got)
	}
	if got, err := mustDoc(t, `{"requests_per_point_multiplier":5}`).RequestsForPoint(Point{Concurrency: 3}); err != nil || got != 15 {
		t.Fatalf("custom multiplier result = %d, want 15", got)
	}
	if got, err := mustDoc(t, `{}`).RequestsForPoint(Point{Concurrency: 3}); err != nil || got != 30 {
		t.Fatalf("omitted multiplier should default to 10, got %d", got)
	}
}

func TestSaturationCanRunFixedAgenticPoints(t *testing.T) {
	s, err := NewJSON("saturation", []byte(`{"fixed_concurrency":[1,2,4,8],"isl":[79998],"osl":[220]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for {
		p, ok := s.Next()
		if !ok {
			break
		}
		got = append(got, p.Concurrency)
		s.Observe(Observation{Point: p, Throughput: float64(p.Concurrency), SuccessRate: 1})
	}
	if len(got) != 4 || got[0] != 1 || got[3] != 8 || s.StopReason() != "hit_max_concurrency" {
		t.Fatalf("fixed points=%v stop=%q", got, s.StopReason())
	}
}

func TestSaturationStopsOnFailure(t *testing.T) {
	s := New(Config{Mode: Saturation, Concurrencies: []int{1}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 8})
	p, _ := s.Next()
	s.Observe(Observation{Point: p, Throughput: 10, SuccessRate: 1})
	p, _ = s.Next()
	if p.Concurrency != 2 {
		t.Fatalf("next = %+v", p)
	}
	s.Observe(Observation{Point: p, Throughput: 11, SuccessRate: .9})
	if _, ok := s.Next(); ok {
		t.Fatal("failure should stop saturation search")
	}
}

func TestSaturationRefinesPlateau(t *testing.T) {
	s := New(Config{Mode: Saturation, Concurrencies: []int{2}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 16})
	p, _ := s.Next()
	s.Observe(Observation{Point: p, Throughput: 10, SuccessRate: 1})
	p, _ = s.Next()
	if p.Concurrency != 4 {
		t.Fatalf("expected doubling, got %+v", p)
	}
	s.Observe(Observation{Point: p, Throughput: 10.1, SuccessRate: 1})
	p, ok := s.Next()
	if !ok || p.Concurrency != 3 {
		t.Fatalf("expected refinement point, got %+v ok=%v", p, ok)
	}
}

func TestAdaptiveStopReasons(t *testing.T) {
	s := New(Config{Mode: Saturation, Concurrencies: []int{1}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 1})
	p, _ := s.Next()
	s.Observe(Observation{Point: p, Throughput: 10, SuccessRate: 1})
	if got := s.StopReason(); got != "hit_max_concurrency" {
		t.Fatalf("saturation stop reason=%q", got)
	}

	s = New(Config{Mode: SLA, Concurrencies: []int{1}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 8, SLA: SLAConfig{MaxP99TTFT: 20}})
	p, _ = s.Next()
	s.Observe(Observation{Point: p, SuccessRate: 1, P99TTFT: 30})
	if got := s.StopReason(); got != "sla_violated" {
		t.Fatalf("SLA stop reason=%q", got)
	}
}

func TestSLAP99Constraint(t *testing.T) {
	s := New(Config{Mode: SLA, Concurrencies: []int{1}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 8, SLA: SLAConfig{MaxP99TTFT: 20}})
	p, _ := s.Next()
	s.Observe(Observation{Point: p, SuccessRate: 1, AvgTTFT: 10, P99TTFT: 30})
	if _, ok := s.Next(); ok {
		t.Fatal("p99 SLA violation must stop")
	}
}

func TestSLABinaryRefinement(t *testing.T) {
	s := New(Config{Mode: SLA, Concurrencies: []int{2}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 16, SLA: SLAConfig{MaxP99TTFT: 20}})
	for _, want := range []int{2, 4, 8} {
		p, ok := s.Next()
		if !ok || p.Concurrency != want {
			t.Fatalf("want %d got %+v", want, p)
		}
		s.Observe(Observation{Point: p, SuccessRate: 1, P99TTFT: 10})
	}
	p, _ := s.Next()
	if p.Concurrency != 16 {
		t.Fatalf("want edge 16 got %+v", p)
	}
	s.Observe(Observation{Point: p, SuccessRate: 1, P99TTFT: 30})
	p, _ = s.Next()
	if p.Concurrency != 12 {
		t.Fatalf("want midpoint 12 got %+v", p)
	}
	s.Observe(Observation{Point: p, SuccessRate: 1, P99TTFT: 30})
	p, _ = s.Next()
	if p.Concurrency != 10 {
		t.Fatalf("want midpoint 10 got %+v", p)
	}
	s.Observe(Observation{Point: p, SuccessRate: 1, P99TTFT: 10})
	p, _ = s.Next()
	if p.Concurrency != 11 {
		t.Fatalf("want midpoint 11 got %+v", p)
	}
	s.Observe(Observation{Point: p, SuccessRate: 1, P99TTFT: 10})
	if _, ok := s.Next(); ok {
		t.Fatal("search should stop at adjacent bounds")
	}
}

func TestNewJSONParsesSLA(t *testing.T) {
	s, err := NewJSON("sla", []byte(`{"concurrency":2,"max_concurrency":8,"isl":[16],"osl":[4],"max_p99_ttft":20,"objective":"rps","min_rps":2,"max_tps":100}`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := s.Next()
	if !ok || p.Concurrency != 2 || p.ISL != 16 || s.cfg.SLA.MaxP99TTFT != 20 || s.cfg.SLA.Objective != "rps" || s.cfg.SLA.MinRPS != 2 || s.cfg.SLA.MaxTPS != 100 {
		t.Fatalf("strategy=%+v point=%+v", s, p)
	}
}

func TestSLARateConstraintsAndObjective(t *testing.T) {
	s := New(Config{Mode: SLA, Concurrencies: []int{1}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 4, SLA: SLAConfig{MinRPS: 2, MaxRPS: 5, MinTPS: 10, MaxTPS: 40, Objective: "rps"}})
	p, _ := s.Next()
	s.Observe(Observation{Point: p, SuccessRate: 1, RequestTput: 1, Throughput: 20})
	if _, ok := s.Next(); ok {
		t.Fatal("below min RPS should not continue")
	}

	s = New(Config{Mode: SLA, Concurrencies: []int{1}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 4, SLA: SLAConfig{Objective: "rps"}})
	p, _ = s.Next()
	s.Observe(Observation{Point: p, SuccessRate: 1, RequestTput: 2, Throughput: 10})
	p, _ = s.Next()
	s.Observe(Observation{Point: p, SuccessRate: 1, RequestTput: 4, Throughput: 9})
	best, ok := s.Best()
	if !ok || best.RequestTput != 4 {
		t.Fatalf("best objective=%+v ok=%v", best, ok)
	}
}

func TestSLALegacyPerUserThroughputConstraints(t *testing.T) {
	s := New(Config{Mode: SLA, Concurrencies: []int{2}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 4, SLA: SLAConfig{LegacyMinPerUserTotalTPS: 30, LegacyMaxPerUserOutputTPS: 7}})
	p, ok := s.Next()
	if !ok {
		t.Fatal("expected initial SLA point")
	}
	s.Observe(Observation{Point: p, SuccessRate: 1, TotalThroughput: 70, Throughput: 16})
	if _, ok := s.Next(); ok {
		t.Fatal("per-user output TPS upper bound should reject the point")
	}

	s = New(Config{Mode: SLA, Concurrencies: []int{2}, ISLs: []int{8}, OSLs: []int{4}, MaxConcurrency: 2, SLA: SLAConfig{LegacyMinPerUserTotalTPS: 30, LegacyMaxPerUserOutputTPS: 8, Objective: "total_tps"}})
	p, _ = s.Next()
	s.Observe(Observation{Point: p, SuccessRate: 1, TotalThroughput: 70, Throughput: 14})
	best, ok := s.Best()
	if !ok || best.TotalThroughput != 70 {
		t.Fatalf("total TPS objective should include prefill: best=%+v ok=%v", best, ok)
	}
}

func TestNewJSONParsesLegacyPerUserTPSAndTotalObjective(t *testing.T) {
	s, err := NewJSON("sla", []byte(`{"concurrency":2,"isl":[16],"osl":[4],"min_per_user_total_tps":12,"max_per_user_output_tps":8,"objective":"total_tps"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.SLA.LegacyMinPerUserTotalTPS != 12 || s.cfg.SLA.LegacyMaxPerUserOutputTPS != 8 || s.cfg.SLA.Objective != "total_tps" {
		t.Fatalf("unexpected SLA config: %+v", s.cfg.SLA)
	}
}
