//go:build linux && dqlite

package app

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// What the admission read costs a small write transaction, on three real
// voters. A measurement, not an assertion: run it with
// GPUWORKS_MEASURE_WRITER_GUARD=1 and read the log. Each write is run with
// the read (the member's Store) and without (a Store on the same database
// that has no member key), alternating, so both see the same load.
func TestNativeWriterGuardCost(t *testing.T) {
	if os.Getenv("GPUWORKS_MEASURE_WRITER_GUARD") == "" {
		t.Skip("measurement only; set GPUWORKS_MEASURE_WRITER_GUARD=1")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	node := store.Node{UUID: "cost-node", Name: "n"}
	if err := f.stores[0].DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	key := store.SettingKey("cost-setting")
	writes := []struct {
		name string
		run  func(*store.Store, int) error
	}{
		{"heartbeat (one update in one transaction)", func(st *store.Store, n int) error {
			return st.TouchNodesAt(map[string]time.Time{node.UUID: time.Unix(int64(1_700_000_000+n), 0).UTC()})
		}},
		{"business write (phase check, one update, phase check), as a chat's key use", func(st *store.Store, n int) error {
			return st.SetBusinessSetting(ctx, key, `"v"`)
		}},
		{"single statement outside a transaction", func(st *store.Store, n int) error {
			return st.DB().Exec("UPDATE nodes SET hostname = ? WHERE uuid = ?", "h", node.UUID).Error
		}},
	}
	const rounds = 400
	summary := func(d []time.Duration, elapsed time.Duration) (time.Duration, time.Duration, float64) {
		slices.Sort(d)
		return d[len(d)/2], d[len(d)*99/100], float64(len(d)) / elapsed.Seconds()
	}
	for index, role := range []string{"on the leader", "on a follower (each statement crosses to the leader)"} {
		member := 0
		if index == 1 {
			member = 2
		}
		with := f.stores[member]
		without, err := store.OpenReplicated(ctx, f.databases[member].SQL, f.databases[member].Scheduling, bus.New(), store.ReplicatedBackend())
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range writes {
			var a, b []time.Duration
			var ta, tb time.Duration
			for n := range rounds {
				started := time.Now()
				if err := w.run(with, n); err != nil {
					t.Fatalf("%s with: %v", w.name, err)
				}
				a, ta = append(a, time.Since(started)), ta+time.Since(started)
				started = time.Now()
				if err := w.run(without, n); err != nil {
					t.Fatalf("%s without: %v", w.name, err)
				}
				b, tb = append(b, time.Since(started)), tb+time.Since(started)
			}
			p50a, p99a, ratea := summary(a, ta)
			p50b, p99b, rateb := summary(b, tb)
			t.Logf("%s, one writer, %s: with the read p50 %v p99 %v %.0f/s; without p50 %v p99 %v %.0f/s; p50 %+.0f%%",
				role, w.name, p50a.Round(10*time.Microsecond), p99a.Round(10*time.Microsecond), ratea,
				p50b.Round(10*time.Microsecond), p99b.Round(10*time.Microsecond), rateb, 100*(float64(p50a)/float64(p50b)-1))
		}
		// Concurrent writers queue for the one database writer, and the read
		// is made while holding it: this is where it costs throughput.
		for _, writers := range []int{8, 16} {
			run := func(st *store.Store) ([]time.Duration, time.Duration) {
				var mu sync.Mutex
				var all []time.Duration
				var group sync.WaitGroup
				started := time.Now()
				for range writers {
					group.Go(func() {
						for n := range 60 {
							begun := time.Now()
							if err := writes[0].run(st, n); err != nil {
								t.Error(err)
								return
							}
							mu.Lock()
							all = append(all, time.Since(begun))
							mu.Unlock()
						}
					})
				}
				group.Wait()
				return all, time.Since(started)
			}
			var a, b []time.Duration
			var ta, tb time.Duration
			for range 2 {
				d, e := run(with)
				a, ta = append(a, d...), ta+e
				d, e = run(without)
				b, tb = append(b, d...), tb+e
			}
			if t.Failed() {
				return
			}
			p50a, p99a, ratea := summary(a, ta)
			p50b, p99b, rateb := summary(b, tb)
			t.Logf("%s, %d concurrent writers, heartbeat: with the read p50 %v p99 %v %.0f/s; without p50 %v p99 %v %.0f/s; throughput %+.0f%%",
				role, writers, p50a.Round(100*time.Microsecond), p99a.Round(100*time.Microsecond), ratea,
				p50b.Round(100*time.Microsecond), p99b.Round(100*time.Microsecond), rateb, 100*(ratea/rateb-1))
		}
		if member != 0 {
			continue
		}
		// The scheduling pool: its transaction now takes the writer and reads
		// before the statement that took the writer anyway.
		var a, b []time.Duration
		var ta, tb time.Duration
		for range rounds / 2 {
			started := time.Now()
			if _, err := with.AdvanceSchedulingGeneration(ctx); err != nil {
				t.Fatal(err)
			}
			a, ta = append(a, time.Since(started)), ta+time.Since(started)
			started = time.Now()
			if _, err := without.AdvanceSchedulingGeneration(ctx); err != nil {
				t.Fatal(err)
			}
			b, tb = append(b, time.Since(started)), tb+time.Since(started)
		}
		p50a, p99a, ratea := summary(a, ta)
		p50b, p99b, rateb := summary(b, tb)
		t.Logf("%s, scheduling pool write: with the read p50 %v p99 %v %.0f/s; without p50 %v p99 %v %.0f/s; p50 %+.0f%%",
			role, p50a.Round(10*time.Microsecond), p99a.Round(10*time.Microsecond), ratea,
			p50b.Round(10*time.Microsecond), p99b.Round(10*time.Microsecond), rateb, 100*(float64(p50a)/float64(p50b)-1))
	}
}
