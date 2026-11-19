package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

var (
	maintWall  = time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC)
	maintAhead = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
)

// asRootAt runs the test's commands as root at wall, invoked through sudo
// by alice.
func asRootAt(t *testing.T, wall time.Time) {
	t.Helper()
	uid, now := effectiveUID, maintenanceNow
	effectiveUID = func() int { return 0 }
	maintenanceNow = func() time.Time { return wall }
	t.Setenv("SUDO_USER", "alice")
	t.Cleanup(func() { effectiveUID, maintenanceNow = uid, now })
}

// storeHWM writes extclock's high-water mark as the server's writer does.
func (e *pluginEnv) storeHWM(at time.Time) {
	e.t.Helper()
	e.withStore(func(st *store.Store) {
		if _, err := st.WritePushedState(context.Background(), &pstate.Batch{Clock: &pstate.Clock{HWMMS: at.UnixMilli()}}); err != nil {
			e.t.Fatal(err)
		}
	})
}

func (e *pluginEnv) journal() string {
	return pluginbridge.MaintenancePath(filepath.Join(e.data, "plugins"))
}

func (e *pluginEnv) entries() []pluginbridge.MaintenanceEntry {
	e.t.Helper()
	entries, errs, err := pluginbridge.ReadMaintenance(e.journal())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, err := range errs {
		if err != nil {
			e.t.Fatalf("the journal holds an entry this build cannot apply: %v", err)
		}
	}
	return entries
}

// Both commands are root's, and refuse while a server runs; neither
// writes the journal when it refuses.
func TestMaintenanceCommandsNeedRootAndAStoppedServer(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.storeHWM(maintAhead)
	uid, now := effectiveUID, maintenanceNow
	t.Cleanup(func() { effectiveUID, maintenanceNow = uid, now })
	effectiveUID = func() int { return 1000 }
	maintenanceNow = func() time.Time { return maintWall }
	commands := [][]string{
		{"clock", "reset", "--to", "2027-01-15T09:00:00Z", "--reason", "clock ran ahead"},
		{"pstate", "clear", "acme_quota", "node_holds", "--reason", "plugin gone"},
	}
	for _, args := range commands {
		if _, err := e.run(args...); err == nil || !strings.Contains(err.Error(), "must run as root") {
			t.Fatalf("%v as a user = %v", args, err)
		}
	}
	effectiveUID = func() int { return 0 }
	release, err := takeServerLock(e.data, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range commands {
		var running *serverRunningError
		if _, err := e.run(args...); !errors.As(err, &running) {
			t.Fatalf("%v with the server running = %v", args, err)
		}
	}
	release()
	if _, err := os.Stat(e.journal()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused command wrote the journal: %v", err)
	}
}

// A reset needs a high-water mark ahead of the wall clock and a target
// between the two, at most 366 days back; what it records is the entry
// the next boot applies.
func TestPluginClockResetIsBoundedAndRecordsOneEntry(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	asRootAt(t, maintWall)
	reset := func(to string) (string, error) {
		return e.run("clock", "reset", "--to", to, "--reason", "clock ran ahead")
	}
	if _, err := reset("2027-01-15T09:00:00Z"); err == nil || !strings.Contains(err.Error(), "not ahead of the wall clock") {
		t.Fatalf("with no high-water mark = %v", err)
	}
	e.storeHWM(maintWall.Add(-time.Minute))
	if _, err := reset("2027-01-15T09:00:00Z"); err == nil || !strings.Contains(err.Error(), "not ahead of the wall clock") {
		t.Fatalf("with the mark behind the wall clock = %v", err)
	}
	e.storeHWM(maintAhead)
	if _, err := reset("2027-01-15T07:59:59Z"); err == nil || !strings.Contains(err.Error(), "before the wall clock") {
		t.Fatalf("before the wall clock = %v", err)
	}
	if _, err := reset("2027-03-01T00:00:01Z"); err == nil || !strings.Contains(err.Error(), "only moves it back") {
		t.Fatalf("after the mark = %v", err)
	}
	if _, err := reset("15 Jan 2027"); err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("not a time = %v", err)
	}
	if _, err := e.run("clock", "reset", "--to", "2027-01-15T09:00:00Z"); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("without a reason = %v", err)
	}
	if _, err := os.Stat(e.journal()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused reset wrote the journal: %v", err)
	}

	// 366 days is the bound of one correction.
	far := maintWall.AddDate(3, 0, 0)
	e.storeHWM(far)
	_, err := reset(far.Add(-366*24*time.Hour - time.Second).Format(time.RFC3339))
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginClockCorrectionTooLarge || pe.Params()["max_days"] != 366 {
		t.Fatalf("beyond 366 days = %v", err)
	}
	out, err := reset(far.Add(-366 * 24 * time.Hour).Format(time.RFC3339))
	if err != nil || !strings.Contains(out, "moves the plugin clock from 2030-01-15T08:00:00Z back to 2029-01-14T08:00:00Z") {
		t.Fatalf("at 366 days = %q %v", out, err)
	}
	got := e.entries()
	want := pluginbridge.MaintenanceEntry{V: 1, Op: pluginbridge.MaintenanceClockReset, At: maintWall, By: "alice",
		To: far.Add(-366 * 24 * time.Hour), Reason: "clock ran ahead"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("journal = %+v, want %+v", got, want)
	}
	info, err := os.Stat(e.journal())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, %v", info, err)
	}
	raw, _ := os.ReadFile(e.journal())
	if string(raw) != `{"v":1,"op":"clock_reset","at":"2027-01-15T08:00:00Z","by":"alice","to":"2029-01-14T08:00:00Z","reason":"clock ran ahead"}`+"\n" {
		t.Fatalf("journal line = %s", raw)
	}
}

// A clear takes a restriction kind only, reports what the owner holds now,
// and appends one entry per run.
func TestPluginPstateClearRecordsOnlyRestrictionKinds(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	asRootAt(t, maintWall)
	e.push("acme_quota")
	for _, kind := range []string{extapi.KindAvailability, extapi.KindBanners, extapi.KindAttestations, "everything"} {
		if _, err := e.run("pstate", "clear", "acme_quota", kind, "--reason", "plugin gone"); productCode(err) != messages.ErrorBadRequest {
			t.Fatalf("clear %s = %v", kind, err)
		}
	}
	if _, err := e.run("pstate", "clear", "../x", "node_holds", "--reason", "x"); err == nil || !strings.Contains(err.Error(), "package id") {
		t.Fatalf("a bad id = %v", err)
	}
	if _, err := e.run("pstate", "clear", "acme_quota", "node_holds"); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("without a reason = %v", err)
	}
	if _, err := os.Stat(e.journal()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused clear wrote the journal: %v", err)
	}
	out, err := e.run("pstate", "clear", "acme_quota", "node_holds", "--reason", "plugin gone")
	if err != nil || !strings.Contains(out, "clears node_holds of acme_quota (1 node holds) and 1 overrides of it") {
		t.Fatalf("clear = %q %v", out, err)
	}
	out, err = e.run("pstate", "clear", "acme_quota", "fleet_capacity", "--reason", "plugin gone")
	if err != nil || !strings.Contains(out, "acme_quota holds no fleet_capacity table now") {
		t.Fatalf("clear of an absent table = %q %v", out, err)
	}
	got := e.entries()
	if len(got) != 2 {
		t.Fatalf("journal = %+v", got)
	}
	want := pluginbridge.MaintenanceEntry{V: 1, Op: pluginbridge.MaintenancePstateClear, At: maintWall, By: "alice",
		Owner: "acme_quota", Kind: extapi.KindNodeHolds, Reason: "plugin gone"}
	if got[0] != want || got[1].Kind != extapi.KindFleetCapacity {
		t.Fatalf("journal = %+v", got)
	}
	// The command recorded; it changed nothing in the database.
	if own := e.owned("acme_quota"); len(own.Rows.NodeHolds) != 1 || len(own.Overrides) != 1 {
		t.Fatalf("the command itself cleared rows: %+v", own)
	}
}
