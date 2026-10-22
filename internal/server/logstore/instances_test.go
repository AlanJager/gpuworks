package logstore

import (
	"fmt"
	"testing"
	"time"
)

// instancesSeed writes fixture rows for three instances (a, b, c) split
// across two UTC-day partitions, so multi-instance queries below are
// exercised across a real partition boundary rather than only within one
// day's table. Arrival order (rowid, which Search/Histogram order by within
// a partition) is a-day1, b-day1, c-day1, then a-day2, b-day2, c-day2.
func instancesSeed(t *testing.T, s Store) (day1, day2 int64) {
	t.Helper()
	d1, err := time.ParseInLocation("20060102-15:04", "20200101-10:00", time.UTC)
	if err != nil {
		t.Fatalf("parse fixture day: %v", err)
	}
	d2 := d1.Add(24 * time.Hour)
	day1, day2 = d1.UnixMilli(), d2.UnixMilli()
	entries := []Entry{
		{Instance: "inst-a", Node: "node-1", TimeMS: day1, Level: LevelInfo, Source: SourceInstance, Line: "a day1"},
		{Instance: "inst-b", Node: "node-2", TimeMS: day1 + 1000, Level: LevelInfo, Source: SourceInstance, Line: "b day1"},
		{Instance: "inst-c", Node: "node-3", TimeMS: day1 + 2000, Level: LevelInfo, Source: SourceInstance, Line: "c day1"},
		{Instance: "inst-a", Node: "node-1", TimeMS: day2, Level: LevelWarn, Source: SourceInstance, Line: "a day2"},
		{Instance: "inst-b", Node: "node-2", TimeMS: day2 + 1000, Level: LevelError, Source: SourceInstance, Line: "b day2"},
		{Instance: "inst-c", Node: "node-3", TimeMS: day2 + 2000, Level: LevelInfo, Source: SourceInstance, Line: "c day2"},
	}
	if err := s.Append(entries); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return day1, day2
}

// TestSearchFiltersByInstanceSetAcrossPartitions proves Filter.Instances
// selects exactly the named instances - not the third one seeded alongside
// them - and that the merged result stays newest-first across the day
// boundary the fixture spans.
func TestSearchFiltersByInstanceSetAcrossPartitions(t *testing.T) {
	s := newStore(t)
	instancesSeed(t, s)

	res, err := s.Search(Filter{Instances: []string{"inst-a", "inst-b"}, Limit: 100})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 4 {
		t.Fatalf("expected 4 entries (a+b across both days), got %d: %+v", len(res.Entries), res.Entries)
	}
	for _, e := range res.Entries {
		if e.Instance != "inst-a" && e.Instance != "inst-b" {
			t.Fatalf("Instances filter leaked entry from %s: %+v", e.Instance, e)
		}
	}
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i-1].TimeMS < res.Entries[i].TimeMS {
			t.Fatalf("Instances filter result not newest-first: %+v", res.Entries)
		}
	}
	// day2 rows (the newest day) must lead the page.
	if res.Entries[0].TimeMS < res.Entries[1].TimeMS {
		t.Fatalf("newest partition did not lead the page: %+v", res.Entries)
	}
}

// TestSearchInstancesCursorPaginatesAcrossPartitionBoundary pages one row at
// a time through the a+b set and checks every row is visited exactly once,
// none from instance c leak in, and the walk actually crosses from the
// day2 partition into day1.
func TestSearchInstancesCursorPaginatesAcrossPartitionBoundary(t *testing.T) {
	s := newStore(t)
	instancesSeed(t, s)

	seen := map[string]bool{}
	var cursorTS, cursorID int64
	pages := 0
	for {
		res, err := s.Search(Filter{Instances: []string{"inst-a", "inst-b"}, Limit: 1, CursorTS: cursorTS, CursorID: cursorID})
		if err != nil {
			t.Fatalf("Search page %d: %v", pages, err)
		}
		if len(res.Entries) == 0 {
			break
		}
		pages++
		for _, e := range res.Entries {
			if e.Instance != "inst-a" && e.Instance != "inst-b" {
				t.Fatalf("cursor page leaked instance %s: %+v", e.Instance, e)
			}
			key := fmt.Sprintf("%s:%d:%d", e.Instance, e.TimeMS, e.ID)
			if seen[key] {
				t.Fatalf("cursor pagination revisited %s", key)
			}
			seen[key] = true
			cursorTS, cursorID = e.TimeMS, e.ID
		}
		if pages > 10 {
			t.Fatal("cursor pagination did not exhaust the corpus")
		}
	}
	if len(seen) != 4 {
		t.Fatalf("cursor pagination saw %d rows, want 4 (a+b across both days)", len(seen))
	}
	if pages != 4 {
		t.Fatalf("expected 4 one-row pages, got %d", pages)
	}
}

// TestHistogramCountsInstanceSet proves Histogram's per-bucket severity
// counts reflect only the named instance set, over the partitions it spans.
func TestHistogramCountsInstanceSet(t *testing.T) {
	s := newStore(t)
	instancesSeed(t, s)

	buckets, err := s.Histogram(Filter{Instances: []string{"inst-a", "inst-b"}}, 24*3600_000)
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	total := int64(0)
	levelCounts := map[string]int64{}
	for _, b := range buckets {
		for level, n := range b.Counts {
			total += n
			levelCounts[level] += n
		}
	}
	if total != 4 {
		t.Fatalf("histogram total = %d, want 4 (a+b only, c excluded)", total)
	}
	if levelCounts[LevelError] != 1 {
		t.Fatalf("levelCounts[error] = %d, want 1 (b's day2 row)", levelCounts[LevelError])
	}
	if levelCounts[LevelWarn] != 1 {
		t.Fatalf("levelCounts[warn] = %d, want 1 (a's day2 row)", levelCounts[LevelWarn])
	}
	if levelCounts[LevelInfo] != 2 {
		t.Fatalf("levelCounts[info] = %d, want 2 (a's and b's day1 rows)", levelCounts[LevelInfo])
	}
}

// TestFilterInstancesMutuallyExclusiveWithInstance pins the validation rule:
// Search and Histogram both reject a filter that sets Instance and Instances
// together, rather than silently picking one.
func TestFilterInstancesMutuallyExclusiveWithInstance(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	if _, err := s.Search(Filter{Instance: "inst-a", Instances: []string{"inst-b"}, Limit: 10}); err == nil {
		t.Fatal("Search with both Instance and Instances set should error")
	}
	if _, err := s.Histogram(Filter{Instance: "inst-a", Instances: []string{"inst-b"}}, 60_000); err == nil {
		t.Fatal("Histogram with both Instance and Instances set should error")
	}
}

// TestFilterInstancesCapped pins MaxInstances as a hard ceiling: one entry
// past it errors on both Search and Histogram, exactly MaxInstances does
// not.
func TestFilterInstancesCapped(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	tooMany := make([]string, MaxInstances+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("inst-%d", i)
	}
	if _, err := s.Search(Filter{Instances: tooMany, Limit: 10}); err == nil {
		t.Fatalf("Search with %d instances (> MaxInstances=%d) should error", len(tooMany), MaxInstances)
	}
	if _, err := s.Histogram(Filter{Instances: tooMany}, 60_000); err == nil {
		t.Fatal("Histogram beyond MaxInstances should error")
	}
	exactly := tooMany[:MaxInstances]
	if _, err := s.Search(Filter{Instances: exactly, Limit: 10}); err != nil {
		t.Fatalf("Search with exactly MaxInstances entries should not error: %v", err)
	}
	if _, err := s.Histogram(Filter{Instances: exactly}, 60_000); err != nil {
		t.Fatalf("Histogram with exactly MaxInstances entries should not error: %v", err)
	}
}

// TestFilterInstancesEmptyBehavesAsUnset proves a zero-length Instances
// slice is indistinguishable from leaving the field unset entirely.
func TestFilterInstancesEmptyBehavesAsUnset(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	unset, err := s.Search(Filter{Limit: 100})
	if err != nil {
		t.Fatalf("Search unset: %v", err)
	}
	empty, err := s.Search(Filter{Instances: []string{}, Limit: 100})
	if err != nil {
		t.Fatalf("Search empty Instances: %v", err)
	}
	if len(unset.Entries) != len(empty.Entries) {
		t.Fatalf("empty Instances behaved differently from unset: %d vs %d entries", len(empty.Entries), len(unset.Entries))
	}
}
