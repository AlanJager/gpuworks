package monitor

import (
	"context"
	"testing"
	"time"

	svmonitor "gpuworks.ai/gpuworks/internal/server/monitor"
)

// TestClusterOverviewStaleFlagTracksNewestSampleAge proves the wire
// contract's "stale" flag (monitor-wire-contract.md: a snapshot whose
// newest sample is older than 30s counts as stale) actually tracks real
// sample staleness end to end through BuildCluster, not just a hand-built
// Window's LatestSampleAt: fresh samples ending at "now" report
// Stale=false, and the same shape of fixture whose samples stopped well in
// the past (mirroring an agent that has gone dark) reports Stale=true once
// "now" advances past them.
func TestClusterOverviewStaleFlagTracksNewestSampleAge(t *testing.T) {
	inv := singleInstanceInventory(1, 1, 1)

	t.Run("fresh samples are not stale", func(t *testing.T) {
		now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		store := newStore(t)
		writeGaugeHold(t, store, "gpuworks_infer_requests_running", map[string]string{"deployment": depKey(1)}, 5, now)
		deps := svmonitor.Deps{
			TSDB: store, Now: func() time.Time { return now },
			Inventory: func(context.Context) (svmonitor.Inventory, error) { return inv, nil },
		}

		cluster, err := svmonitor.BuildCluster(context.Background(), deps, time.Hour)
		if err != nil {
			t.Fatalf("BuildCluster: %v", err)
		}
		if cluster.Stale {
			t.Fatalf("cluster.Stale = true with a sample at now, want false (AsOf=%v now=%v)", cluster.AsOf, now)
		}
		if !cluster.AsOf.Equal(now) {
			t.Fatalf("cluster.AsOf = %v, want exactly now (%v)", cluster.AsOf, now)
		}
	})

	t.Run("samples that stopped read as stale", func(t *testing.T) {
		// 2 minutes stays inside DiagnosisWindow's 5-minute lookback (so
		// BuildCluster's query still finds the sample at all) while comfortably
		// exceeding staleAfter (30s), so the snapshot reads as stale rather than
		// simply "no data" (which would report a zero AsOf instead).
		now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		sampleTime := now.Add(-2 * time.Minute)
		store := newStore(t)
		writeGaugeHold(t, store, "gpuworks_infer_requests_running", map[string]string{"deployment": depKey(1)}, 5, sampleTime)
		deps := svmonitor.Deps{
			TSDB: store, Now: func() time.Time { return now },
			Inventory: func(context.Context) (svmonitor.Inventory, error) { return inv, nil },
		}

		cluster, err := svmonitor.BuildCluster(context.Background(), deps, time.Hour)
		if err != nil {
			t.Fatalf("BuildCluster: %v", err)
		}
		if !cluster.Stale {
			t.Fatalf("cluster.Stale = false 10m after the last sample, want true (AsOf=%v now=%v)", cluster.AsOf, now)
		}
		if !cluster.AsOf.Equal(sampleTime) {
			t.Fatalf("cluster.AsOf = %v, want the last real sample time %v", cluster.AsOf, sampleTime)
		}
	})
}
