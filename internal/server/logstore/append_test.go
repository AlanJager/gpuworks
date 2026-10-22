package logstore

import "testing"

// TestAppendSingleBatchSpanningTwoDaysRoutesEachRowToItsOwnPartition is a
// regression test: Append used to insert every entry of a batch into every
// day's partition table it touched (a `for _, e := range entries` where it
// should have ranged over that day's own subset), which was invisible as
// long as no single Append call ever carried entries from two different UTC
// days at once - every existing fixture happened to avoid that. The agent's
// real ingest batches (up to 1000 lines or 200ms of buffering) can easily
// straddle a UTC midnight boundary, so a batch spanning two days must route
// each row to exactly the one partition its own timestamp belongs to.
func TestAppendSingleBatchSpanningTwoDaysRoutesEachRowToItsOwnPartition(t *testing.T) {
	s := newStore(t)
	day1, day2 := instancesSeed(t, s) // one Append call, 3 rows per day, two distinct UTC days

	res, err := s.Search(Filter{FromMS: day1, ToMS: day1 + 3600_000, Limit: 100})
	if err != nil {
		t.Fatalf("Search day1 window: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("day1 partition holds %d rows, want exactly the 3 day1 entries (not day2's leaking in): %+v", len(res.Entries), res.Entries)
	}
	for _, e := range res.Entries {
		if e.TimeMS < day1 || e.TimeMS >= day2 {
			t.Fatalf("day1 window returned a row outside day1: %+v", e)
		}
	}

	res, err = s.Search(Filter{FromMS: day2, ToMS: day2 + 3600_000, Limit: 100})
	if err != nil {
		t.Fatalf("Search day2 window: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("day2 partition holds %d rows, want exactly the 3 day2 entries (not day1's leaking in): %+v", len(res.Entries), res.Entries)
	}

	// Total across both partitions must be 6, not 12 - each row lives in
	// exactly one partition table.
	all, err := s.Search(Filter{Limit: 100})
	if err != nil {
		t.Fatalf("Search all: %v", err)
	}
	if len(all.Entries) != 6 {
		t.Fatalf("total rows = %d, want 6 (each of the 6 seeded entries stored exactly once)", len(all.Entries))
	}
}
