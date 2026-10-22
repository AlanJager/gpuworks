package search

import (
	"fmt"
	"testing"
)

func TestAdaptiveConcurrencyStopsAtRepresentableMaximum(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	start := maxInt/2 + 1
	for _, mode := range []string{"sla", "saturation"} {
		for _, limit := range []int{start + 1, 0, maxInt} {
			t.Run(fmt.Sprintf("%s/max=%d", mode, limit), func(t *testing.T) {
				doc := mustDoc(t, fmt.Sprintf(`{"concurrency":%d,"max_concurrency":%d,"isl":[1],"osl":[1],"requests_per_point_multiplier":1e-18}`, start, limit))
				planner, err := NewDocument(mode, doc)
				if err != nil {
					t.Fatal(err)
				}
				end := limit
				if end == 0 {
					end = maxInt
				}
				for i, want := range []int{start, end} {
					point, ok := planner.Next()
					if !ok || point.Concurrency != want {
						t.Fatalf("point[%d]=%+v ok=%v want=%d", i, point, ok, want)
					}
					count, err := doc.RequestsForPoint(point)
					if err != nil || count < 1 || count > 10 {
						t.Fatalf("count=%d err=%v", count, err)
					}
					planner.Observe(Observation{Point: point, SuccessRate: 1, Throughput: float64(i+1) * 100})
				}
				if point, ok := planner.Next(); ok {
					t.Fatalf("unexpected point after maximum: %+v", point)
				}
				if planner.StopReason() != "hit_max_concurrency" {
					t.Fatalf("stop reason=%q", planner.StopReason())
				}
			})
		}
	}
}

func TestAdaptiveBisectionNearIntegerLimit(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	start := maxInt/2 + 1
	for _, mode := range []string{"sla", "saturation"} {
		t.Run(mode, func(t *testing.T) {
			planner, err := NewDocument(mode, mustDoc(t, fmt.Sprintf(`{"concurrency":%d,"max_concurrency":%d,"max_p99_ttft":1}`, start, maxInt)))
			if err != nil {
				t.Fatal(err)
			}
			first, _ := planner.Next()
			planner.Observe(Observation{Point: first, SuccessRate: 1, Throughput: 100, P99TTFT: .5})
			upper, _ := planner.Next()
			planner.Observe(Observation{Point: upper, SuccessRate: 0, P99TTFT: 2})
			middle, ok := planner.Next()
			want := start + (maxInt-start)/2
			if !ok || middle.Concurrency != want {
				t.Fatalf("middle=%+v ok=%v want=%d", middle, ok, want)
			}
		})
	}
}
