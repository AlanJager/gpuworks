package collector

import (
	"testing"
	"time"
)

func TestRoleSamplerReportsRatesFromTheCounters(t *testing.T) {
	totals := map[string][2]uint64{"storage_sync": {1_000, 2_000}}
	r := roleSampler{totals: func() map[string][2]uint64 {
		out := map[string][2]uint64{}
		for k, v := range totals {
			out[k] = v
		}
		return out
	}}
	at := time.Unix(100, 0)
	if got := r.sample("node-1", at, 1); len(got) != 0 {
		t.Fatalf("first sample has no baseline: %+v", got)
	}
	totals["storage_sync"] = [2]uint64{51_000, 2_000}
	got := r.sample("node-1", at.Add(10*time.Second), 2)
	if len(got) != 2 {
		t.Fatalf("samples = %+v", got)
	}
	rx, tx := got[0], got[1]
	if rx.Metric != "gpuworks_node_role_rx_bytes_per_sec" || rx.Value != 5_000 || rx.Labels["role"] != "storage_sync" || rx.Labels["node"] != "node-1" || rx.TimeMS != 2 {
		t.Fatalf("rx = %+v", rx)
	}
	if tx.Metric != "gpuworks_node_role_tx_bytes_per_sec" || tx.Value != 0 {
		t.Fatalf("an idle direction reports zero: %+v", tx)
	}
	// A counter that goes backwards (never in one process, but defensively)
	// and a clock that does not advance report nothing bogus.
	totals["storage_sync"] = [2]uint64{10, 10}
	if got := r.sample("node-1", at.Add(10*time.Second), 3); len(got) != 0 {
		t.Fatalf("same instant = %+v", got)
	}
	totals["storage_sync"] = [2]uint64{0, 0}
	for _, s := range r.sample("node-1", at.Add(20*time.Second), 4) {
		if s.Value != 0 {
			t.Fatalf("reset counter = %+v", s)
		}
	}
	if (&roleSampler{}).sample("node-1", at, 5) != nil {
		t.Fatal("no counters, no samples")
	}
}
