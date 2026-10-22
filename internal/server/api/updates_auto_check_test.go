package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// autoCheckServer is a server.yaml-backed settings fake for the automatic
// update check switch.
type autoCheckServer struct {
	StaticServerSettings
	manual bool
	id     string
	saves  []bool
}

func (s *autoCheckServer) AutoCheckUpdates() (bool, bool) { return !s.manual, true }
func (s *autoCheckServer) SetAutoCheckUpdates(on bool) error {
	s.manual = !on
	s.saves = append(s.saves, on)
	return nil
}
func (s *autoCheckServer) InstallID() string { return s.id }

// reportingRun makes the channel one of the built-in sources, the only kind
// that receives the report, and seeds a small fleet.
func reportingRun(t *testing.T, server *autoCheckServer, usage ...*fakeUsageReport) (*imageManifestRun, *testChannel) {
	t.Helper()
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	saved := distribution.DefaultSources
	distribution.DefaultSources = []string{channel.server.URL}
	t.Cleanup(func() { distribution.DefaultSources = saved })
	run.deps.Outside = OutsideSources{Channel: &distribution.Client{
		Keys: distribution.Keys{"test": channelTestPublic}, IndexTimeout: 5 * time.Second, StallTimeout: 5 * time.Second,
	}}
	run.deps.Server = server
	run.deps.UsageReport = &fakeUsageReport{enabled: true}
	if len(usage) > 0 {
		run.deps.UsageReport = usage[0]
	}
	run.router = Router(run.deps)

	cardDefaultNode(t, run.store, "report-a", nvidiaCard(0, "A100", "8.0", 80), nvidiaCard(1, "A100", "8.0", 80))
	cardDefaultNode(t, run.store, "report-b", nvidiaCard(0, "H20", "9.0", 96))
	cardDefaultNode(t, run.store, "report-c") // registered, never connected, no cards
	for uuid, agent := range map[string]string{"report-a": "v0.9.1", "report-b": "v0.9.1"} {
		if err := run.store.SetNodeAgentInfo(uuid, agent, ""); err != nil {
			t.Fatal(err)
		}
	}
	for uuid, state := range map[string]string{"report-a": store.NodeStateOnline, "report-b": store.NodeStateOffline, "report-c": store.NodeStateConnecting} {
		if err := run.store.SetNodeState(uuid, state); err != nil {
			t.Fatal(err)
		}
	}
	return run, channel
}

// An automatic check describes the fleet by exactly these parameters.
func TestAutomaticUpdateCheckTellsTheChannelTheFleet(t *testing.T) {
	run, channel := reportingRun(t, &autoCheckServer{id: "0123456789abcdef"})
	status := getUpdateStatus(t, run)
	if !status.AutoCheck || !status.Available {
		t.Fatalf("status = %+v", status)
	}
	queries := channel.queryLog()
	if len(queries) != 1 {
		t.Fatalf("the sources were asked %d times", len(queries))
	}
	got, err := url.ParseQuery(queries[0])
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"id": {"0123456789abcdef"}, "v": {version.Version}, "nodes": {"3"}, "online": {"1"},
		"agent": {"v0.9.1:2", "unknown:1"},
		"gpu":   {"A100:2", "H20:1"},
	}
	for key, values := range want {
		if strings.Join(got[key], "|") != strings.Join(values, "|") {
			t.Errorf("%s = %v, want %v (query %q)", key, got[key], values, queries[0])
		}
	}
	if len(got) != len(want) {
		t.Errorf("the report carries other parameters than %v: %q", want, queries[0])
	}
}

// Out of the improvement program, an automatic check still names the
// installation and its versions (so installations can be counted) but says
// nothing about the fleet's size or cards.
func TestAutomaticUpdateCheckOutsideTheProgramNamesOnlyVersions(t *testing.T) {
	for name, usage := range map[string]*fakeUsageReport{"switched off": {enabled: false}, "not wired": nil} {
		t.Run(name, func(t *testing.T) {
			run, channel := reportingRun(t, &autoCheckServer{id: "0123456789abcdef"}, usage)
			if usage == nil {
				run.deps.UsageReport = nil
				run.router = Router(run.deps)
			}
			if status := getUpdateStatus(t, run); !status.AutoCheck || !status.Available {
				t.Fatalf("status = %+v", status)
			}
			queries := channel.queryLog()
			if len(queries) != 1 {
				t.Fatalf("the sources were asked %d times", len(queries))
			}
			got, err := url.ParseQuery(queries[0])
			if err != nil {
				t.Fatal(err)
			}
			want := url.Values{"id": {"0123456789abcdef"}, "v": {version.Version}, "agent": {"v0.9.1:2", "unknown:1"}}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("the check carried %v, want %v", got, want)
			}
		})
	}
}

// With automatic checks off, nothing is asked on a page load or a sign-in
// page; a manual check asks and says nothing about the fleet.
func TestUpdatesWithAutoCheckOffAskOnlyWhenAsked(t *testing.T) {
	run, channel := reportingRun(t, &autoCheckServer{manual: true, id: "0123456789abcdef"})

	rec := httptest.NewRecorder()
	run.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/updates/prefetch", nil))
	time.Sleep(100 * time.Millisecond)
	if rec.Code != http.StatusNoContent || len(channel.requested()) != 0 {
		t.Fatalf("prefetch = %d, requests %v", rec.Code, channel.requested())
	}

	status := getUpdateStatus(t, run)
	if status.AutoCheck || status.Available || status.UnavailableReason != "" || len(status.Sources) != 0 || len(channel.requested()) != 0 {
		t.Fatalf("a page load with checks off = %+v, requests %v", status, channel.requested())
	}

	rec = run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":true`) || !strings.Contains(rec.Body.String(), `"auto_check":false`) {
		t.Fatalf("manual check = %d %s", rec.Code, rec.Body.String())
	}
	if queries := channel.queryLog(); len(queries) != 1 || queries[0] != "" {
		t.Fatalf("the manual check carried %q, want nothing", queries)
	}

	// The next page load shows what the manual check found, without asking.
	if status := getUpdateStatus(t, run); !status.Available || len(channel.requested()) != 1 {
		t.Fatalf("page load after a manual check = %+v, requests %v", status, channel.requested())
	}
}

func TestPutUpdateAutoCheck(t *testing.T) {
	server := &autoCheckServer{}
	run := newImageManifestRun(t)
	run.deps.Server = server
	run.router = Router(run.deps)
	rec := run.do(http.MethodPut, "/api/v1/updates/auto-check", "application/json", `{"enabled":false}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":false}` || len(server.saves) != 1 || server.saves[0] {
		t.Fatalf("switch off = %d %s, saves %v", rec.Code, rec.Body.String(), server.saves)
	}
	if status := getUpdateStatus(t, run); status.AutoCheck {
		t.Fatalf("status after the switch = %+v", status)
	}
	rec = run.do(http.MethodPut, "/api/v1/updates/auto-check", "application/json", `{"enabled":true}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":true}` {
		t.Fatalf("switch on = %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{}`, `{"enabled":"yes"}`, `not json`} {
		rec := run.do(http.MethodPut, "/api/v1/updates/auto-check", "application/json", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorUpdateAutoCheckInvalid) {
			t.Fatalf("PUT %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(server.saves) != 2 {
		t.Fatalf("rejected requests were saved: %v", server.saves)
	}
	// Without server.yaml there is nowhere to keep the choice.
	run.deps.Server = StaticServerSettings{}
	run.router = Router(run.deps)
	rec = run.do(http.MethodPut, "/api/v1/updates/auto-check", "application/json", `{"enabled":false}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorUpdateAutoCheckReadOnly) {
		t.Fatalf("read-only switch = %d %s", rec.Code, rec.Body.String())
	}
	if rec := httptest.NewRecorder(); true {
		run.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/updates/auto-check", strings.NewReader(`{"enabled":false}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous switch = %d", rec.Code)
		}
	}
}
