//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

type fakeUpgradeProgress struct {
	status    store.ClusterUpgradeStatus
	migrating bool
	err       error
	calls     int
}

func (f *fakeUpgradeProgress) ClusterUpgradeProgress(context.Context) (store.ClusterUpgradeStatus, bool, error) {
	f.calls++
	return f.status, f.migrating, f.err
}

func upgradeFacts(phase string, applied, known int, members ...store.ClusterUpgradeMember) store.ClusterUpgradeStatus {
	return store.ClusterUpgradeStatus{Phase: store.MaintenancePhaseState{Phase: phase}, Applied: applied, Known: known, AppliedExtensions: 2, KnownExtensions: 2, Members: members}
}

// The public progress document is literal wire JSON: counts and a state
// code, and nothing that names a member, a release or an error.
func TestClusterUpgradeProgressDocumentWire(t *testing.T) {
	on := store.ClusterUpgradeMember{KeyFingerprint: "secret-fingerprint", Address: "10.0.0.1:9001", Reported: true, Markers: 246, Last: "drop_node_placement_policy", Extensions: 2, Fresh: true}
	behind := store.ClusterUpgradeMember{KeyFingerprint: "other-fingerprint", Address: "10.0.0.2:9001", Reported: true, Markers: 245, Extensions: 2}
	silent := store.ClusterUpgradeMember{Address: "10.0.0.3:9001"}
	cases := []struct {
		name     string
		source   *fakeUpgradeProgress
		open     bool
		wantWire string
	}{
		{"idle and serving", &fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceServing, 246, 246, on, on, on)}, true,
			`{"state":"idle","business_open":true,"schema":{"applied":248,"known":248},"members":{"total":3,"on_target":3,"fresh":3}}`},
		{"finished: the reports are gone and every member counts", &fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceServing, 246, 246, silent, silent, silent)}, false,
			`{"state":"idle","business_open":false,"schema":{"applied":248,"known":248},"members":{"total":3,"on_target":3,"fresh":0}}`},
		{"new executable waits for the window", &fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceServing, 245, 246, on, behind, silent)}, false,
			`{"state":"waiting","business_open":false,"schema":{"applied":247,"known":248},"members":{"total":3,"on_target":1,"fresh":1}}`},
		{"window open", &fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceUpgrading, 245, 246, on, on, on)}, false,
			`{"state":"window","business_open":false,"schema":{"applied":247,"known":248},"members":{"total":3,"on_target":3,"fresh":3}}`},
		{"migration running", &fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceUpgrading, 245, 246, on, on, on), migrating: true}, false,
			`{"state":"migrating","business_open":false,"schema":{"applied":247,"known":248},"members":{"total":3,"on_target":3,"fresh":3}}`},
		{"facts unreadable", &fakeUpgradeProgress{err: errors.New("no leader: 10.0.0.1:9001 refused")}, false,
			`{"state":"unobserved","business_open":false,"schema":{"applied":0,"known":0},"members":{"total":0,"on_target":0,"fresh":0}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			newClusterUpgradeProgress(tc.source).serve(w, httptest.NewRequest(http.MethodGet, ClusterUpgradeProgressPath, nil), tc.open)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("answer: %d %v", w.Code, w.Header())
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.wantWire {
				t.Fatalf("wire\n got %s\nwant %s", got, tc.wantWire)
			}
		})
	}
}

// Any number of anonymous readers costs one database read a second, and the
// document is read-only.
func TestClusterUpgradeProgressSharesOneObservation(t *testing.T) {
	source := &fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceUpgrading, 245, 246)}
	progress := newClusterUpgradeProgress(source)
	now := time.Unix(1_800_000_000, 0)
	progress.now = func() time.Time { return now }
	for range 50 {
		progress.serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, ClusterUpgradeProgressPath, nil), false)
	}
	if source.calls != 1 {
		t.Fatalf("50 readers in one second made %d observations", source.calls)
	}
	now = now.Add(time.Second)
	progress.serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, ClusterUpgradeProgressPath, nil), false)
	if source.calls != 2 {
		t.Fatalf("after a second: %d observations", source.calls)
	}
	// The cached observation still reports this request's own business state.
	w := httptest.NewRecorder()
	progress.serve(w, httptest.NewRequest(http.MethodGet, ClusterUpgradeProgressPath, nil), true)
	if !strings.Contains(w.Body.String(), `"business_open":true`) {
		t.Fatalf("business state was cached: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	progress.serve(w, httptest.NewRequest(http.MethodPost, ClusterUpgradeProgressPath, nil), false)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet || source.calls != 2 {
		t.Fatalf("POST: %d allow %q observations %d", w.Code, w.Header().Get("Allow"), source.calls)
	}
}

// A closed member answers the progress document, and a browser that opens a
// page during the window gets the self-contained progress page on the same
// 503 an entry layer reads as not ready. API, gateway and download paths and
// non-browser clients keep the coded answer; outside the window nothing
// changes.
func TestClosedGateServesUpgradeProgressAndPage(t *testing.T) {
	gate := newBusinessHandlerGate()
	gate.upgradeProgress = newClusterUpgradeProgress(&fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceUpgrading, 245, 246)})
	answer := func(method, path, accept string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, r)
		return w
	}
	const browser = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	if w := answer(http.MethodGet, ClusterUpgradeProgressPath, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"window","business_open":false`) {
		t.Fatalf("progress on a closed member: %d %s", w.Code, w.Body.String())
	}
	if w := answer(http.MethodGet, "/settings", browser); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("closed member outside the window: %d %q", w.Code, w.Body.String())
	}
	gate.clusterUpgrade.Store(true)
	for _, path := range []string{"/", "/settings", "/deployments/7"} {
		w := answer(http.MethodGet, path, browser)
		body := w.Body.String()
		if w.Code != http.StatusServiceUnavailable || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Retry-After") != "5" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", path, w.Code, w.Header())
		}
		if !strings.Contains(body, ClusterUpgradeProgressPath) || !strings.Contains(body, "控制面正在升级") || !strings.Contains(body, "The control plane is being upgraded") {
			t.Fatalf("%s: page does not poll the progress document in both languages", path)
		}
		if strings.Contains(body, "src=") || strings.Contains(body, "href=") {
			t.Fatalf("%s: page refers to an asset the closed member cannot serve", path)
		}
	}
	for _, c := range []struct{ method, path, accept string }{
		{http.MethodGet, "/api/v1/nodes", browser}, {http.MethodGet, "/v1/models", browser}, {http.MethodGet, "/dl/gpuworks-agent-linux-amd64", browser},
		{http.MethodGet, "/settings", "application/json"}, {http.MethodGet, "/settings", ""}, {http.MethodPost, "/settings", browser},
	} {
		w := answer(c.method, c.path, c.accept)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"CLUSTER_UPGRADE_IN_PROGRESS"`) {
			t.Fatalf("%s %s accept %q: %d %q", c.method, c.path, c.accept, w.Code, w.Body.String())
		}
	}
	if w := answer(http.MethodGet, "/healthz", browser); w.Code != http.StatusOK {
		t.Fatalf("liveness: %d", w.Code)
	}
}

// An open member passes the progress path to nothing else, and reports its
// business as open.
func TestOpenGateReportsBusinessOpen(t *testing.T) {
	gate := newBusinessHandlerGate()
	gate.upgradeProgress = newClusterUpgradeProgress(&fakeUpgradeProgress{status: upgradeFacts(store.MaintenanceServing, 246, 246)})
	reached := false
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) }))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest(http.MethodGet, ClusterUpgradeProgressPath, nil))
	if reached || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"idle","business_open":true`) {
		t.Fatalf("open member: reached business %v, %d %s", reached, w.Code, w.Body.String())
	}
}
