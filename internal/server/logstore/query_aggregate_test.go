package logstore

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Match the console's seven-day request, including its default source and
// with_total. Checking only keyword pages misses the expensive count plan.
func TestKeywordAggregatesAcrossPartitions(t *testing.T) {
	s := newStore(t)
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC).UnixMilli()
	const dayMS = int64(24 * time.Hour / time.Millisecond)
	for day := int64(0); day < 7; day++ {
		base := Entry{Instance: "inst-a", Node: "node-a", Source: SourceInstance, Level: LevelError, TimeMS: start + day*dayMS + 60_000, Line: "Traceback: (failed)"}
		entries := []Entry{base}
		other := base
		other.Line = "Traceback: (retry)"
		other.Level = LevelFatal
		entries = append(entries, other)
		for _, change := range []func(*Entry){
			func(e *Entry) { e.Source = SourceAgent },
			func(e *Entry) { e.Node = "node-b" },
			func(e *Entry) { e.Instance = "inst-b" },
			func(e *Entry) { e.Level = LevelInfo },
			func(e *Entry) { e.Line = "healthy" },
			func(e *Entry) { e.TimeMS = start - dayMS },
			func(e *Entry) { e.TimeMS = start + 7*dayMS },
		} {
			other = base
			change(&other)
			entries = append(entries, other)
		}
		if err := s.Append(entries); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query  string
		counts map[string]int64
	}{
		{"Traceback", map[string]int64{LevelError: 1, LevelFatal: 1}},
		{`"Traceback failed"`, map[string]int64{LevelError: 1}},
		{"failed OR retry", map[string]int64{LevelError: 1, LevelFatal: 1}},
		{"Traceback: (failed)", map[string]int64{LevelError: 1}},
		{"absent", nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			f := Filter{Query: tc.query, Source: SourceInstance, Node: "node-a", Instance: "inst-a", Level: LevelError, FromMS: start, ToMS: start + 7*dayMS - 1, Limit: 1, WithTotal: true}
			page, err := s.Search(f)
			if err != nil {
				t.Fatal(err)
			}
			wantTotal := int64(7 * len(tc.counts))
			if page.Total != wantTotal || page.TotalCapped || page.Truncated != (wantTotal > 1) {
				t.Fatalf("unexpected page: %+v; want total %d", page, wantTotal)
			}
			buckets, err := s.Histogram(f, dayMS)
			if err != nil {
				t.Fatal(err)
			}
			wantBuckets := 7
			if wantTotal == 0 {
				wantBuckets = 0
			}
			if len(buckets) != wantBuckets {
				t.Fatalf("got %d buckets, want %d: %+v", len(buckets), wantBuckets, buckets)
			}
			for i, b := range buckets {
				if b.StartMS != start+int64(6-i)*dayMS || !reflect.DeepEqual(b.Counts, tc.counts) {
					t.Fatalf("unexpected bucket: %+v", b)
				}
			}
			// Page through exactly the same console filters. The rowid sort
			// must survive the source index and time-range predicates.
			f.WithTotal = false
			var previous Entry
			seen := int64(0)
			for {
				page, err := s.Search(f)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Entries) == 0 {
					break
				}
				entry := page.Entries[0]
				if seen > 0 && (entry.TimeMS > previous.TimeMS || (entry.TimeMS == previous.TimeMS && entry.ID >= previous.ID)) {
					t.Fatalf("pagination not newest-first: previous=%+v next=%+v", previous, entry)
				}
				seen++
				if seen > wantTotal {
					t.Fatal("pagination returned duplicate or non-matching rows")
				}
				previous = entry
				f.CursorTS, f.CursorID = entry.TimeMS, entry.ID
			}
			if seen != wantTotal {
				t.Fatalf("pagination lost matches: got %d, want %d", seen, wantTotal)
			}
		})
	}
}

func TestKeywordQueriesStartWithFTS(t *testing.T) {
	s := newStore(t).(*sqliteStore)
	seed(t, s)
	days, err := listPartitions(s.reader)
	if err != nil {
		t.Fatal(err)
	}
	f := Filter{Query: "CUDA", Source: SourceInstance, FromMS: 1}
	pq := compile(days[0], f)
	for _, query := range []string{pq.countSQL(TotalCap), pq.selectSQL(201, true), pq.selectSQL(201, false)} {
		assertFTSFirstPlan(t, s.reader, query, pq.args)
	}
}

func assertFTSFirstPlan(t *testing.T, db *sql.DB, query string, args []any) {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var accesses []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(detail, "SCAN ") || strings.HasPrefix(detail, "SEARCH ") {
			accesses = append(accesses, detail)
		}
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("keyword page/count must stream without sorting: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Assert the engine's actual loop order, not the SQL spelling or a
	// wall-clock threshold that becomes flaky on busy CI workers.
	if len(accesses) < 2 || !strings.Contains(accesses[0], "VIRTUAL TABLE") || !strings.Contains(accesses[1], "INTEGER PRIMARY KEY") {
		t.Fatalf("keyword query must read FTS hits before metadata: %v", accesses)
	}
}

func TestCountLimitAppliesToMatches(t *testing.T) {
	s := newStore(t).(*sqliteStore)
	seed(t, s)
	days, err := listPartitions(s.reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "CUDA"} {
		for _, limit := range []int64{0, 1, 2, 10} {
			pq := compile(days[0], Filter{Query: query, Source: SourceInstance, Level: LevelWarn, FromMS: 1})
			var n int64
			if err := s.reader.QueryRow(pq.countSQL(limit), pq.args...).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if want := min(limit, 2); n != want {
				t.Fatalf("query=%q limit=%d: count=%d, want %d", query, limit, n, want)
			}
		}
	}
}

// planDetails returns EXPLAIN QUERY PLAN's detail lines for query.
func planDetails(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

// The console's default first screen - instance lines, newest first, from an
// open-ended start - walks the partition from its newest row instead of
// sorting every matching line; a selective filter keeps its index. Both read
// the same rows as the indexed form of the same query.
func TestBroadNewestFirstPageStreamsFromTheTail(t *testing.T) {
	s := newStore(t).(*sqliteStore)
	seed(t, s)
	days, err := listPartitions(s.reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []Filter{{Source: SourceInstance, FromMS: 1}, {FromMS: 1}, {Source: SourceInstance}} {
		pq := compile(days[0], f)
		if pq.tailWhere == nil {
			t.Fatalf("%+v: not read from the tail", f)
		}
		for _, detail := range planDetails(t, s.reader, pq.selectSQL(201, true), pq.args) {
			if strings.Contains(detail, "TEMP B-TREE") || strings.HasPrefix(detail, "SEARCH") {
				t.Fatalf("%+v: broad page plan %q", f, detail)
			}
		}
		tail, err := scanEntries(s.reader.Query(pq.selectSQL(201, true), pq.args...))
		if err != nil {
			t.Fatal(err)
		}
		indexed := pq
		indexed.tailWhere = nil
		want, err := scanEntries(s.reader.Query(indexed.selectSQL(201, true), indexed.args...))
		if err != nil {
			t.Fatal(err)
		}
		if len(tail) == 0 || len(tail) != len(want) {
			t.Fatalf("%+v: tail read %d rows, indexed %d", f, len(tail), len(want))
		}
		for i := range want {
			if tail[i].ID != want[i].ID {
				t.Fatalf("%+v: row %d id %d, want %d", f, i, tail[i].ID, want[i].ID)
			}
		}
	}
	for _, f := range []Filter{{Instance: "inst-a", Source: SourceInstance, FromMS: 1}, {Node: "n1", FromMS: 1},
		{Source: SourceAgent, FromMS: 1}, {Source: SourceInstance, FromMS: 1, ToMS: 1 << 62}, {Query: "CUDA", Source: SourceInstance}} {
		if pq := compile(days[0], f); pq.tailWhere != nil {
			t.Fatalf("%+v: a selective filter must keep its index", f)
		}
	}
}
