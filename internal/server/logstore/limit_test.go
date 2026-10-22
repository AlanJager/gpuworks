package logstore

import (
	"testing"
	"time"
)

// bulkAppend writes n single-instance rows in one Append call, timestamped
// one millisecond apart starting at "now" (UTC), for the limit/truncation
// tests below that need a corpus larger than any page they request.
func bulkAppend(t *testing.T, s Store, instance string, n int) {
	t.Helper()
	base := time.Now().UTC().UnixMilli()
	entries := make([]Entry, n)
	for i := 0; i < n; i++ {
		entries[i] = Entry{Instance: instance, Node: "node-1", TimeMS: base + int64(i), Level: LevelInfo, Source: SourceInstance, Line: "bulk line"}
	}
	if err := s.Append(entries); err != nil {
		t.Fatalf("Append %d rows: %v", n, err)
	}
}

// TestSearchTruncatesAtExplicitLimitBelowMax proves an explicit limit well
// under MaxLimit is honored exactly (not silently re-clamped to some lower
// internal ceiling), and Result.Truncated reports the rows left behind.
func TestSearchTruncatesAtExplicitLimitBelowMax(t *testing.T) {
	s := newStore(t)
	bulkAppend(t, s, "inst-trunc", 3000)

	res, err := s.Search(Filter{Instance: "inst-trunc", Limit: 2000})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 2000 {
		t.Fatalf("Search returned %d entries, want exactly the requested 2000", len(res.Entries))
	}
	if !res.Truncated {
		t.Fatal("Truncated should be true: 3000 rows exist behind a 2000-row page")
	}
}

// TestSearchClampsLimitToMaxLimit proves a limit above MaxLimit is clamped
// to it, and Truncated is still reported correctly at that boundary (the
// exact case that broke before: a limit+1 probe layered on top of a hard
// cap could lose the signal right at the ceiling).
func TestSearchClampsLimitToMaxLimit(t *testing.T) {
	s := newStore(t)
	bulkAppend(t, s, "inst-cap", MaxLimit+5)

	res, err := s.Search(Filter{Instance: "inst-cap", Limit: MaxLimit + 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != MaxLimit {
		t.Fatalf("Search returned %d entries, want exactly MaxLimit=%d (clamped)", len(res.Entries), MaxLimit)
	}
	if !res.Truncated {
		t.Fatal("Truncated should be true: more rows exist beyond MaxLimit")
	}
}

// TestSearchDefaultLimitAppliedWhenUnset proves an unset (<=0) Limit
// resolves to DefaultLimit rather than erroring or returning everything
// unbounded.
func TestSearchDefaultLimitAppliedWhenUnset(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	res, err := s.Search(Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 5 {
		t.Fatalf("expected all 5 seeded entries under the default limit, got %d", len(res.Entries))
	}
	if res.Truncated {
		t.Fatal("Truncated should be false: far fewer rows than DefaultLimit")
	}
}

// TestSearchNotTruncatedWhenExactlyAtLimit is the boundary case the probe
// exists to get right: a match set exactly the size of the page must not be
// reported as truncated.
func TestSearchNotTruncatedWhenExactlyAtLimit(t *testing.T) {
	s := newStore(t)
	seed(t, s) // 5 entries total
	res, err := s.Search(Filter{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 5 || res.Truncated {
		t.Fatalf("Search = %d entries, Truncated=%v; want exactly 5 and Truncated=false", len(res.Entries), res.Truncated)
	}
}
