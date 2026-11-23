//go:build linux && dqlite

package app

import (
	"context"
	"os"
	"slices"
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
			for n := range rounds {
				started := time.Now()
				if err := w.run(with, n); err != nil {
					t.Fatalf("%s with: %v", w.name, err)
				}
				a = append(a, time.Since(started))
				started = time.Now()
				if err := w.run(without, n); err != nil {
					t.Fatalf("%s without: %v", w.name, err)
				}
				b = append(b, time.Since(started))
			}
			summary := func(d []time.Duration) (time.Duration, time.Duration, float64) {
				slices.Sort(d)
				var total time.Duration
				for _, v := range d {
					total += v
				}
				return d[len(d)/2], d[len(d)*99/100], float64(len(d)) / total.Seconds()
			}
			p50a, p99a, ratea := summary(a)
			p50b, p99b, rateb := summary(b)
			t.Logf("%s, %s: with the read p50 %v p99 %v %.0f/s; without p50 %v p99 %v %.0f/s; p50 +%.0f%%",
				role, w.name, p50a.Round(10*time.Microsecond), p99a.Round(10*time.Microsecond), ratea,
				p50b.Round(10*time.Microsecond), p99b.Round(10*time.Microsecond), rateb, 100*(float64(p50a)/float64(p50b)-1))
		}
	}
}
