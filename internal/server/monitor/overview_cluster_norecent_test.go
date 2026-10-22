package monitor

import (
	"context"
	"testing"
	"time"
)

// This file covers ClusterSignals.ProxyNoRecent/TTFTNoRecent/TPOTNoRecent
// end to end through BuildCluster: the three states a family can be in
// relative to DiagnosisWindow and the wider chart range (rng) -
// "never collected" (no series anywhere), "collected+recent" (traffic
// inside DiagnosisWindow) and "collected+idle window" (traffic exists
// somewhere in rng but not in the last five minutes, the state that used to
// misreport collected=false). The wire-level detail.no_recent_requests
// rendering itself is covered at the monitorapi layer
// (TestSignalsDTONoRecentRequests*); this file only proves BuildCluster's
// own Has*/NoRecent/value plumbing that feeds it.

// TestBuildClusterProxyFamilyThreeStates covers request_rate/error_rate.
func TestBuildClusterProxyFamilyThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sampleAge time.Duration // how far before "now" the sample run ends; < 0 means "write nothing"
	}{
		{name: "never collected", sampleAge: -1},
		{name: "collected recent", sampleAge: 30 * time.Second},
		{name: "collected idle window", sampleAge: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			inv := Inventory{
				Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
				Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
			}
			if tc.sampleAge >= 0 {
				end := baseTime.Add(-tc.sampleAge)
				labels := map[string]string{"deployment": "1", "class": "2xx"}
				writeSample(t, store, "gpuworks_proxy_requests_total", labels, 90, end.Add(-10*time.Second))
				writeSample(t, store, "gpuworks_proxy_requests_total", labels, 100, end)
			}
			deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

			snap, err := BuildCluster(context.Background(), deps, time.Hour)
			if err != nil {
				t.Fatalf("BuildCluster: %v", err)
			}

			collected := tc.sampleAge >= 0
			// DiagnosisWindow is 5 minutes: a sample run ending inside it
			// is "recent", one older than it (but still inside the 1h rng
			// this test queries) is "idle window".
			recentInWindow := collected && tc.sampleAge <= DiagnosisWindow
			wantNoRecent := collected && !recentInWindow

			if snap.Signals.HasRequestRate != collected {
				t.Errorf("Signals.HasRequestRate = %v, want %v", snap.Signals.HasRequestRate, collected)
			}
			if snap.Signals.HasErrRate != collected {
				t.Errorf("Signals.HasErrRate = %v, want %v (same family as request_rate)", snap.Signals.HasErrRate, collected)
			}
			if snap.Signals.ProxyNoRecent != wantNoRecent {
				t.Errorf("Signals.ProxyNoRecent = %v, want %v", snap.Signals.ProxyNoRecent, wantNoRecent)
			}
			switch {
			case recentInWindow:
				if snap.Signals.RequestsPerSec <= 0 {
					t.Errorf("Signals.RequestsPerSec = %v, want > 0 for recent traffic", snap.Signals.RequestsPerSec)
				}
			case wantNoRecent:
				if snap.Signals.RequestsPerSec != 0 {
					t.Errorf("Signals.RequestsPerSec = %v, want exactly 0 in the quiet-window state", snap.Signals.RequestsPerSec)
				}
				if snap.Signals.ErrRateTotalPct != 0 {
					t.Errorf("Signals.ErrRateTotalPct = %v, want exactly 0 in the quiet-window state", snap.Signals.ErrRateTotalPct)
				}
			}

			if len(snap.Services) != 1 {
				t.Fatalf("Services = %+v, want 1", snap.Services)
			}
			row := snap.Services[0]
			// Service rows: request_rate 0 (not null) whenever this
			// deployment's own proxy series exist anywhere in rng -
			// collected exactly mirrors the fleet-wide flag here since
			// there is only one deployment.
			if row.HasRequestRate != collected {
				t.Errorf("Services[0].HasRequestRate = %v, want %v", row.HasRequestRate, collected)
			}
			if wantNoRecent && row.RequestsPerSec != 0 {
				t.Errorf("Services[0].RequestsPerSec = %v, want exactly 0 in the quiet-window state", row.RequestsPerSec)
			}
		})
	}
}

// TestBuildClusterProxyFamilyFlatCounterInWindowIsNotNoRecent pins the
// distinct "flat, not absent" case dto.MonitorSignal's doc comment calls
// out: at least two proxy samples land inside DiagnosisWindow but the
// counter simply did not increase between them (traffic paused mid-window).
// The window answered the question with a real zero - it is not the "no
// sample at all in the window" case ProxyNoRecent exists for - so
// HasRequestRate/HasErrRate must be true the ordinary way (not via the
// widened-range fallback) and ProxyNoRecent must stay false even though
// RequestsPerSec reads exactly 0.
func TestBuildClusterProxyFamilyFlatCounterInWindowIsNotNoRecent(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	// Two samples, both inside DiagnosisWindow, same cumulative value: a
	// real window measurement of zero growth, not a missing one.
	labels := map[string]string{"deployment": "1", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", labels, 100, baseTime.Add(-90*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", labels, 100, baseTime.Add(-10*time.Second))
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if !snap.Signals.HasRequestRate {
		t.Fatal("Signals.HasRequestRate = false, want true (the window itself has two samples to answer from)")
	}
	if snap.Signals.RequestsPerSec != 0 {
		t.Errorf("Signals.RequestsPerSec = %v, want exactly 0 (flat counter)", snap.Signals.RequestsPerSec)
	}
	if snap.Signals.ProxyNoRecent {
		t.Error("Signals.ProxyNoRecent = true, want false: the window has samples, it just measured zero growth - not the same as no sample at all")
	}
	if len(snap.Services) != 1 {
		t.Fatalf("Services = %+v, want 1", snap.Services)
	}
	if row := snap.Services[0]; !row.HasRequestRate || row.RequestsPerSec != 0 {
		t.Errorf("Services[0] = %+v, want HasRequestRate=true RequestsPerSec=0", row)
	}
}

// TestBuildClusterTTFTFamilyThreeStates covers the ttft/tpot signals' own
// widened-range check (they only need Has*/NoRecent at the fleet level -
// ServiceRow.TTFT deliberately never widens, since a service row carries no
// detail to explain a stale value - see BuildCluster's own comments).
func TestBuildClusterTTFTFamilyThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sampleAge time.Duration
	}{
		{name: "never collected", sampleAge: -1},
		{name: "collected recent", sampleAge: 30 * time.Second},
		{name: "collected idle window", sampleAge: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			inv := Inventory{
				Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
				Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
			}
			if tc.sampleAge >= 0 {
				at := baseTime.Add(-tc.sampleAge)
				labels := map[string]string{"deployment": "1", "instance": "i1", "quantile": "0.95"}
				writeSample(t, store, "gpuworks_infer_ttft_seconds", labels, 2.5, at)
			}
			deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

			snap, err := BuildCluster(context.Background(), deps, time.Hour)
			if err != nil {
				t.Fatalf("BuildCluster: %v", err)
			}

			collected := tc.sampleAge >= 0
			recentInWindow := collected && tc.sampleAge <= DiagnosisWindow
			wantNoRecent := collected && !recentInWindow

			if snap.Signals.HasTTFT != collected {
				t.Errorf("Signals.HasTTFT = %v, want %v", snap.Signals.HasTTFT, collected)
			}
			if snap.Signals.TTFTNoRecent != wantNoRecent {
				t.Errorf("Signals.TTFTNoRecent = %v, want %v", snap.Signals.TTFTNoRecent, wantNoRecent)
			}
			if wantNoRecent && snap.Signals.TTFTP95Ms != 0 {
				t.Errorf("Signals.TTFTP95Ms = %v, want exactly 0 (no fresh sample this window to reduce)", snap.Signals.TTFTP95Ms)
			}
			if recentInWindow && snap.Signals.TTFTP95Ms <= 0 {
				t.Errorf("Signals.TTFTP95Ms = %v, want > 0 for a recent sample", snap.Signals.TTFTP95Ms)
			}

			if len(snap.Services) != 1 {
				t.Fatalf("Services = %+v, want 1", snap.Services)
			}
			// ServiceRow.HasTTFT never widens: it stays exactly the
			// un-widened per-deployment window truth, "quiet window" or not.
			if row := snap.Services[0]; row.HasTTFT != recentInWindow {
				t.Errorf("Services[0].HasTTFT = %v, want %v (row must never widen)", row.HasTTFT, recentInWindow)
			}
		})
	}
}
