package logstore

import (
	"reflect"
	"testing"
)

// The console combines the whole text expression with OR-ed quick signatures.
// Exercise those wire queries against actual FTS5, with the same node, source
// and fixed window for search, totals and histogram.
func TestCombinedTextAndQuickFilters(t *testing.T) {
	s := newStore(t)
	const ts int64 = 1792108800000
	lines := []string{"WARNING cache miss alpha", "WARNING cache warm alpha", "ERROR CUDA cache miss: out of memory", "INFO cache miss"}
	var entries []Entry
	for _, line := range lines {
		level := LevelWarn
		if line == lines[2] {
			level = LevelError
		}
		if line == lines[3] {
			level = LevelInfo
		}
		entries = append(entries, Entry{Instance: "inst-a", Node: "node-a", TimeMS: ts, Source: SourceInstance, Level: level, Line: line})
	}
	entries = append(entries,
		Entry{Instance: "inst-b", Node: "node-b", TimeMS: ts, Source: SourceInstance, Level: LevelWarn, Line: lines[0]},
		Entry{Node: "node-a", TimeMS: ts, Source: SourceAgent, Level: LevelWarn, Line: lines[0]},
		Entry{Instance: "inst-a", Node: "node-a", TimeMS: ts - 60000, Source: SourceInstance, Level: LevelWarn, Line: lines[0]})
	if err := s.Append(entries); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		ids         []int64
		counts      map[string]int64
	}{
		{"text only", "WARNING", []int64{2, 1}, map[string]int64{LevelWarn: 2}},
		{"one quick signature", "(WARNING) AND WARNING", []int64{2, 1}, map[string]int64{LevelWarn: 2}},
		{"multiple quick signatures", "(WARNING) AND (Traceback OR ERROR OR WARNING)", []int64{2, 1}, map[string]int64{LevelWarn: 2}},
		{"quoted text", `("cache miss") AND (ERROR OR WARNING)`, []int64{3, 1}, map[string]int64{LevelWarn: 1, LevelError: 1}},
		{"text OR cannot bypass quick filter", "(WARNING OR CUDA) AND ERROR", []int64{3}, map[string]int64{LevelError: 1}},
		{"quoted signature", `(cache) AND "out of memory"`, []int64{3}, map[string]int64{LevelError: 1}},
		{"no intersection", "(WARNING) AND ERROR", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Filter{Query: tc.query, Node: "node-a", Instance: "inst-a", Source: SourceInstance, FromMS: ts - 1, ToMS: ts + 1, WithTotal: true}
			result, err := s.Search(f)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, entry := range result.Entries {
				ids = append(ids, entry.ID)
			}
			if !reflect.DeepEqual(ids, tc.ids) || result.Total != int64(len(tc.ids)) || result.Truncated || result.TotalCapped {
				t.Fatalf("query %q: result=%+v want IDs=%v", tc.query, result, tc.ids)
			}
			buckets, err := s.Histogram(f, 60000)
			if err != nil {
				t.Fatal(err)
			}
			if tc.counts == nil {
				if len(buckets) != 0 {
					t.Fatalf("empty search histogram=%+v", buckets)
				}
				return
			}
			if len(buckets) != 1 || buckets[0].StartMS != ts || !reflect.DeepEqual(buckets[0].Counts, tc.counts) {
				t.Fatalf("histogram=%+v want counts=%v", buckets, tc.counts)
			}
		})
	}
}
