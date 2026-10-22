package search

import (
	"reflect"
	"testing"
)

func mustDoc(t *testing.T, raw string) Document {
	t.Helper()
	doc, err := ParseDocument([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestOpenLoopGridExpandsRatesWithInFlightCap(t *testing.T) {
	doc := mustDoc(t, `{"concurrency":[1,2,4],"request_rates":[0.5,2],"max_in_flight":16,"isl":[128],"osl":[32,64]}`)
	got, err := doc.Expand()
	if err != nil {
		t.Fatal(err)
	}
	// Rates replace concurrency as the point dimension; the concurrency list
	// left over from closed-loop editing is ignored.
	want := []Point{
		{Concurrency: 16, ISL: 128, OSL: 32, RequestRate: 0.5}, {Concurrency: 16, ISL: 128, OSL: 64, RequestRate: 0.5},
		{Concurrency: 16, ISL: 128, OSL: 32, RequestRate: 2}, {Concurrency: 16, ISL: 128, OSL: 64, RequestRate: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("points = %+v", got)
	}
}

func TestOpenLoopRequestsForPointUsesDurationAndFloor(t *testing.T) {
	cases := []struct {
		raw  string
		rate float64
		want int
	}{
		{`{"request_rates":[2]}`, 2, 120},                          // default 60 s of arrivals
		{`{"request_rates":[2.5],"point_duration_s":10}`, 2.5, 25}, // explicit duration
		{`{"request_rates":[0.1],"point_duration_s":10}`, 0.1, 20}, // floor
		{`{"request_rates":[0.3],"point_duration_s":70}`, 0.3, 21}, // rounds up
	}
	for _, tc := range cases {
		doc := mustDoc(t, tc.raw)
		if got, err := doc.RequestsForPoint(Point{RequestRate: tc.rate}); err != nil || got != tc.want {
			t.Fatalf("%s rate %v: requests = %d, want %d", tc.raw, tc.rate, got, tc.want)
		}
	}
}

func TestClosedLoopDocumentKeepsConcurrencyGrid(t *testing.T) {
	doc := mustDoc(t, `{"concurrency":3,"isl":[9],"osl":[5],"warmup_requests":2}`)
	got, err := doc.Expand()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Point{{Concurrency: 3, ISL: 9, OSL: 5}}) || doc.OpenLoop() || doc.WarmupRequests != 2 {
		t.Fatalf("points = %+v doc = %+v", got, doc)
	}
	if doc.EffectiveBurstiness() != 1 || doc.EffectivePointDuration() != 60 {
		t.Fatalf("defaults burstiness=%v duration=%v", doc.EffectiveBurstiness(), doc.EffectivePointDuration())
	}
}

func TestSLAInteractivityUsesMedianTPOT(t *testing.T) {
	sla := mustDoc(t, `{"min_interactivity":40}`).SLA()
	// 1000 / 20 ms = 50 tok/s/user meets the floor; 1000 / 30 ms = 33.3 fails.
	if v := sla.Violations(Observation{P50TPOT: 20, P99TPOT: 90}); len(v) != 0 {
		t.Fatalf("violations = %+v", v)
	}
	v := sla.Violations(Observation{P50TPOT: 30})
	if len(v) != 1 || v[0].Metric != "interactivity" || v[0].Limit != 40 || v[0].Actual < 33.33 || v[0].Actual > 33.34 {
		t.Fatalf("violations = %+v", v)
	}
	// A point with no TPOT (single-chunk responses) has no measured speed.
	if v := sla.Violations(Observation{}); len(v) != 1 {
		t.Fatalf("missing TPOT should fail the floor: %+v", v)
	}
}

func TestSLALegacyPerUserKeysKeepThroughputPerConcurrency(t *testing.T) {
	sla := mustDoc(t, `{"min_per_user_output_tps":10,"max_per_user_total_tps":50}`).SLA()
	o := Observation{Point: Point{Concurrency: 4}, Throughput: 36, TotalThroughput: 240, P50TPOT: 10}
	got := sla.Violations(o)
	want := []Violation{{Metric: "per_user_total_tps", Limit: 50, Actual: 60, Reason: "limit_exceeded"}, {Metric: "per_user_output_tps", Limit: 10, Actual: 9, Reason: "limit_exceeded"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("violations = %+v", got)
	}
}
