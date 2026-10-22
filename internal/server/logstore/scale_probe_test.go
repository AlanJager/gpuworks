package logstore

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The scale probe measures the store shapes the product promises at fleet
// scale: ingest rate, first-screen tail latency, keyword search, and retention
// cost. It is a load measurement, so it is gated the same way the fleet cases
// are, and runs on an idle host on request:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v ./internal/server/logstore/ -run '^TestScaleProbe$'
//
// Rows are synthetic but shaped like real container logs — mostly INFO access
// noise (the /metrics heartbeat pattern dominates unfiltered production
// stores), a WARN band, and a small ERROR/Traceback tail — so match rates and
// scan depths are representative. Baseline numbers against the pre-v2 schema
// (single FTS5 table, UNINDEXED ts, one connection) are recorded in
// artifacts/archive/2026-09-19-log-center/plan.md.
func TestScaleProbe(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("logstore load probe; set GPUWORKS_TEST_PERF=1 on an idle host to run")
	}
	for _, size := range []int{10_000, 100_000, 500_000} {
		t.Run(fmt.Sprintf("rows=%d", size), func(t *testing.T) {
			st, err := OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer st.Close()

			// Ingest.
			start := time.Now()
			const batch = 64
			buf := make([]Entry, 0, batch)
			for i := 0; i < size; i++ {
				buf = append(buf, sampleEntry(i))
				if len(buf) == batch {
					if err := st.Append(buf); err != nil {
						t.Fatalf("append: %v", err)
					}
					buf = buf[:0]
				}
			}
			if err := st.Append(buf); err != nil {
				t.Fatalf("append: %v", err)
			}
			ingest := time.Since(start)
			t.Logf("ingest %d rows in %v (%.0f rows/s)", size, ingest, float64(size)/ingest.Seconds())

			probe := func(name string, fn func()) {
				// A few passes; report the median-ish (third of five).
				var d [5]time.Duration
				for i := range d {
					s := time.Now()
					fn()
					d[i] = time.Since(s)
				}
				slices.Sort(d[:])
				t.Logf("%-34s %v (median of 5)", name, d[2])
			}

			probe("recent (first screen, no q)", func() {
				if _, err := st.Search(Filter{Limit: 200}); err != nil {
					t.Fatalf("recent: %v", err)
				}
			})
			probe(`search "error"`, func() {
				if _, err := st.Search(Filter{Query: "error", Limit: 200}); err != nil {
					t.Fatalf("search: %v", err)
				}
			})
			// The console always includes source and requests totals on its
			// first page; that count can dominate an otherwise cheap search.
			consoleFilter := Filter{Query: "Traceback", Source: SourceInstance, FromMS: time.Now().Add(-7 * 24 * time.Hour).UnixMilli(), Limit: 200, WithTotal: true}
			probe("seven-day Traceback with total", func() {
				if _, err := st.Search(consoleFilter); err != nil {
					t.Fatalf("console search: %v", err)
				}
			})
			probe("seven-day Traceback histogram", func() {
				if _, err := st.Histogram(consoleFilter, 3600_000); err != nil {
					t.Fatalf("console histogram: %v", err)
				}
			})
			probe(`search "GET /metrics" (60%)`, func() {
				if _, err := st.Search(Filter{Query: `"GET /metrics"`, Limit: 200}); err != nil {
					t.Fatalf("search: %v", err)
				}
			})
			probe(`recent instance-prefix`, func() {
				if _, err := st.Search(Filter{Instance: "6f9a1c2d", Limit: 200}); err != nil {
					t.Fatalf("recent: %v", err)
				}
			})
			probe(`search "error" instance-prefix`, func() {
				if _, err := st.Search(Filter{Query: "error", Instance: "6f9a1c2d", Limit: 200}); err != nil {
					t.Fatalf("search: %v", err)
				}
			})
			probe("recent level>=warn", func() {
				if _, err := st.Search(Filter{Level: LevelWarn, Limit: 200}); err != nil {
					t.Fatalf("recent: %v", err)
				}
			})
			probe("histogram 60s buckets", func() {
				if _, err := st.Histogram(Filter{}, 60_000); err != nil {
					t.Fatalf("histogram: %v", err)
				}
			})

			// Retention: delete the oldest 20% and time it.
			cutoff := time.Now().Add(-6 * 24 * time.Hour).UnixMilli()
			s := time.Now()
			if err := st.Retain(context.Background(), cutoff); err != nil {
				t.Fatalf("retain: %v", err)
			}
			t.Logf("retain (delete oldest ~%d rows) in %v", size*2/10, time.Since(s))
		})
	}
}

var sampleInstances = []string{"6f9a1c2d", "b41d90ee", "02aa77f3", "cd9012ab", "7e553311"}

// sampleEntry builds the i-th synthetic line. ~60% is /metrics access noise,
// ~30% plain INFO/WARN engine chatter, ~10% ERROR/Traceback frames.
func sampleEntry(i int) Entry {
	r := rand.New(rand.NewSource(int64(i))) // deterministic per index
	var line, level string
	switch p := r.Intn(100); {
	case p < 60:
		line = fmt.Sprintf("(APIServer pid=%d) INFO: 127.0.0.1:%d - \"GET /metrics HTTP/1.1\" 200 OK", 10000+i%50, 30000+i%20000)
		level = LevelInfo
	case p < 90:
		line = fmt.Sprintf("(APIServer pid=%d) INFO 09-18 10:%02d:%02d [core.py:%d] Request complete: %s", 10000+i%50, i%60, i%60, 500+i%400, sampleInstances[i%len(sampleInstances)])
		level = LevelInfo
	case p < 97:
		line = fmt.Sprintf("(APIServer pid=%d) WARNING 09-18 10:%02d:%02d [config.py:%d] Unknown vLLM environment variable detected: VLLM_%d", 10000+i%50, i%60, i%60, 100+i%50, i%7)
		level = LevelWarn
	default:
		line = fmt.Sprintf("(APIServer pid=%d) ERROR 09-18 10:%02d:%02d [core.py:%d] Traceback (most recent call last): frame %d", 10000+i%50, i%60, i%60, 900+i%99, i)
		level = LevelError
	}
	return Entry{
		Instance: sampleInstances[i%len(sampleInstances)],
		Node:     fmt.Sprintf("node-%04d", i%40),
		Level:    level,
		Source:   SourceInstance,
		TimeMS:   time.Now().Add(-time.Duration(i) * time.Millisecond).UnixMilli(),
		Line:     line,
	}
}
