package tsdb

import "testing"

func TestQueryLatestBoundsBeforeSelectingNewestPerSeries(t *testing.T) {
	s := openTemp(t)
	if err := s.WriteBatch([]Sample{
		sample("0", 999, 1), sample("0", 1000, 2), sample("0", 2000, 3), sample("0", 2001, 98), sample("0", 60_000, 99),
		sample("1", 999, 4),  // stale only
		sample("2", 2001, 5), // future only
		sample("3", 1000, 6), // inclusive lower boundary
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryLatest(metricUtil, nil, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("bounded latest = %+v", got)
	}
	for _, p := range got {
		switch p.Labels["gpu"] {
		case "0":
			if p.TimeMS != 2000 || p.Value != 3 {
				t.Fatalf("upper boundary masked by future samples: %+v", p)
			}
		case "3":
			if p.TimeMS != 1000 || p.Value != 6 {
				t.Fatalf("lower boundary excluded: %+v", p)
			}
		default:
			t.Fatalf("out-of-window series: %+v", p)
		}
	}
	// Original callers retain their lower-bound-only semantics.
	unbounded, err := s.QueryLatest(metricUtil, map[string]string{"gpu": "0"}, 1000)
	if err != nil || len(unbounded) != 1 || unbounded[0].TimeMS != 60_000 {
		t.Fatalf("unbounded latest=%+v error=%v", unbounded, err)
	}
	empty, err := s.QueryLatest(metricUtil, nil, 2000, 1000)
	if err != nil || len(empty) != 0 {
		t.Fatalf("reversed window=%+v error=%v", empty, err)
	}
	if _, err := s.QueryLatest(metricUtil, nil, 1000, 2000, 3000); err == nil {
		t.Fatal("multiple upper bounds must be rejected")
	}
}

func TestQueryLatestBoundsPreserveIndexedAndLabelMatchers(t *testing.T) {
	s := openTemp(t)
	for _, inst := range []string{"wanted", "other"} {
		for _, stamp := range []int64{1000, 2000, 3000} {
			p := sample("0", stamp, stamp)
			p.Labels["instance"] = inst
			p.Labels["deployment"] = "7"
			if err := s.WriteBatch([]Sample{p}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.QueryLatest(metricUtil, map[string]string{"instance": "wanted", "deployment": "7", "gpu": "0"}, 1000, 2000)
	if err != nil || len(got) != 1 || got[0].TimeMS != 2000 || got[0].Labels["instance"] != "wanted" {
		t.Fatalf("matched latest=%+v error=%v", got, err)
	}
}
