package pluginbridge

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func (w *world) journal() string { return MaintenancePath(w.dir + "/plugins") }

func (w *world) record(e MaintenanceEntry) {
	w.t.Helper()
	if err := os.MkdirAll(w.dir+"/plugins", 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := AppendMaintenance(w.journal(), e); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) journalText() string {
	w.t.Helper()
	raw, err := os.ReadFile(w.journal())
	if err != nil {
		w.t.Fatal(err)
	}
	return string(raw)
}

// pushAs pushes body as the installed package id, bound as the running
// plugin would be.
func pushAs(t *testing.T, w *world, b *Bridge, id string, perms, codes []string, body string) {
	t.Helper()
	b.mu.Lock()
	b.pushers[id] = identity{version: "1.0.0", pusher: pushstate.Pusher{ID: id, Publisher: "acme",
		Grants: w.pub.Cert.Grants, Permissions: perms, Codes: codes}}
	b.mu.Unlock()
	push, err := extapi.DecodeStatePush([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	owner := pluginhost.Owner{ID: id, Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 1}
	if _, err := (stateHost{b}).Push(context.Background(), owner, http.MethodPut, push); err != nil {
		t.Fatal(err)
	}
}

func shownCode(b *Bridge) string {
	for _, bn := range b.state.Banners(false) {
		if bn.Notice != nil {
			return bn.Notice.Code()
		}
	}
	return ""
}

func maintenanceBanner(b *Bridge) *pushstate.Banner {
	for _, bn := range b.HealthBanners() {
		if bn.Notice.Code() == messages.ErrorPluginMaintenanceFailed {
			return &bn
		}
	}
	return nil
}

// The offline `plugin clock reset` path (A13): a step that fired while the
// clock ran ahead un-fires at the next boot, before any plugin starts,
// with the correction's banner naming who reset it, its audit line, and
// the corrected high-water mark in what the host API's meta reads. The
// journal is empty afterwards, so a later boot applies nothing again.
func TestABootAppliesAnOfflineClockResetAndUnfiresAStep(t *testing.T) {
	w := newWorld(t, nil)
	ahead := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	clk := &settableClock{wall: ahead}
	var effects []string
	corrected := func(from, to time.Time, by, reason string) {
		effects = append(effects, from.Format(time.RFC3339)+">"+to.Format(time.RFC3339)+" by "+by+": "+reason)
	}
	b, stop := openOnClock(t, w, clk, corrected)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	pushAs(t, w, b, "acme_backup", []string{"push:banners"}, []string{"ACME_EARLY", "ACME_LATE"},
		`{"body":{"banners":[{"audience":"all","id":"term","steps":[`+
			`{"notice":{"code":"ACME_EARLY"},"severity":"info"},`+
			`{"at":"2027-02-01T00:00:00Z","notice":{"code":"ACME_LATE"},"severity":"warning"}]}]},"generation":1,"kind":"banners","schema":1}`)
	if got := shownCode(b); got != "ACME_LATE" {
		t.Fatalf("at the clock that ran ahead the banner shows %q", got)
	}
	stop()

	// The operator puts the wall clock right and records the reset with
	// the server stopped, as `gpuworks plugin clock reset` does.
	clk.set(right)
	w.record(MaintenanceEntry{V: MaintenanceVersion, Op: MaintenanceClockReset, At: right, By: "alice", To: right, Reason: "clock ran ahead"})
	b, stop = openOnClock(t, w, clk, corrected)
	if got := shownCode(b); got != "ACME_EARLY" {
		t.Fatalf("after the boot applied the reset the banner shows %q, want the earlier step", got)
	}
	if _, ext, hwm := (stateHost{b}).Clocks(); !ext.Equal(right) || !hwm.Equal(right) {
		t.Fatalf("the host API's meta reads extclock %v and high-water mark %v, want %v", ext, hwm, right)
	}
	var notice *messages.ProductError
	for _, bn := range b.state.Banners(true) {
		if bn.Notice.Code() == messages.ErrorPluginClockCorrected {
			notice = bn.Notice
		}
	}
	if notice == nil || notice.Params()["by"] != "alice" || notice.Params()["to"] != "2027-01-15T00:00:00Z" || notice.Params()["from"] != "2027-03-01T00:00:00Z" {
		t.Fatalf("clock banner = %+v", notice)
	}
	if len(effects) != 1 || effects[0] != "2027-03-01T00:00:00Z>2027-01-15T00:00:00Z by alice: clock ran ahead" {
		t.Fatalf("correction effects = %v", effects)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, `msg="offline maintenance applied" audit=true op=clock_reset by=alice reason="clock ran ahead"`) {
		t.Fatalf("no audit line of the applied entry:\n%s", logs)
	}
	if text := w.journalText(); text != "" || maintenanceBanner(b) != nil {
		t.Fatalf("after the boot the journal holds %q and the banner is %+v", text, maintenanceBanner(b))
	}
	stop()

	// Nothing applies twice.
	b, _ = openOnClock(t, w, clk, corrected)
	if len(effects) != 1 || shownCode(b) != "ACME_EARLY" {
		t.Fatalf("a second boot applied the reset again: %v", effects)
	}
}

// The offline `plugin pstate clear` path: the boot releases the owner's
// restriction table before any plugin starts and empties the journal.
func TestABootAppliesAnOfflineClear(t *testing.T) {
	w := newWorld(t, nil)
	clk := &settableClock{wall: time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)}
	b, stop := openOnClock(t, w, clk, nil)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	pushAs(t, w, b, "acme_backup", []string{"push:node_holds", "push:banners"}, []string{"ACME_HELD"},
		`{"body":{"nodes":{"7":{"code":"ACME_HELD"}}},"generation":3,"kind":"node_holds","schema":1}`)
	pushAs(t, w, b, "acme_backup", []string{"push:node_holds", "push:banners"}, []string{"ACME_HELD"},
		`{"body":{"banners":[{"audience":"admin","id":"held","steps":[{"notice":{"code":"ACME_HELD"},"severity":"info"}]}]},"generation":1,"kind":"banners","schema":1}`)
	if b.state.Hold(7, 8).Code == "" {
		t.Fatal("the pushed hold is not enforced")
	}
	stop()
	w.record(MaintenanceEntry{V: MaintenanceVersion, Op: MaintenancePstateClear, At: clk.wall, By: "root", Owner: "acme_backup", Kind: extapi.KindNodeHolds, Reason: "plugin gone"})
	b, _ = openOnClock(t, w, clk, nil)
	if b.state.Hold(7, 8).Code != "" {
		t.Fatal("the boot did not release the cleared hold")
	}
	if held := b.state.Held("acme_backup"); len(held) != 1 || held[0].Kind != extapi.KindBanners {
		t.Fatalf("after the clear acme_backup holds %+v", held)
	}
	if !strings.Contains(w.logs.String(), `msg="offline maintenance applied" audit=true op=pstate_clear by=root reason="plugin gone"`) {
		t.Fatalf("no audit line of the clear:\n%s", w.logs.String())
	}
	if text := w.journalText(); text != "" {
		t.Fatalf("journal after the boot = %q", text)
	}
}

// The first entry that fails stops the journal: the entries before it
// apply and leave it, it and the ones after it stay for the next boot,
// and the administrator banner names its operation and code. An entry the
// build cannot read fails the same way.
func TestAFailedMaintenanceEntryStaysAndRaisesTheBanner(t *testing.T) {
	w := newWorld(t, nil)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	clk := &settableClock{wall: right}
	w.record(MaintenanceEntry{V: MaintenanceVersion, Op: MaintenancePstateClear, At: right, By: "root", Owner: "acme_backup", Kind: extapi.KindNodeHolds, Reason: "first"})
	w.record(MaintenanceEntry{V: MaintenanceVersion, Op: MaintenanceClockReset, At: right, By: "root", To: right.AddDate(2, 0, 0), Reason: "too far"})
	w.record(MaintenanceEntry{V: MaintenanceVersion, Op: MaintenancePstateClear, At: right, By: "root", Owner: "acme_other", Kind: extapi.KindFleetCapacity, Reason: "after"})
	lines := strings.SplitAfter(w.journalText(), "\n")

	b, stop := openOnClock(t, w, clk, nil)
	bn := maintenanceBanner(b)
	if bn == nil || bn.Severity != extapi.SeverityWarning || bn.Notice.Params()["op"] != MaintenanceClockReset ||
		bn.Notice.Params()["code"] != messages.ErrorPluginClockCorrectionTooLarge {
		t.Fatalf("maintenance banner = %+v", bn)
	}
	if text := w.journalText(); text != lines[1]+lines[2] {
		t.Fatalf("journal after the failure = %q, want the failed entry and the one after it", text)
	}
	if logs := w.logs.String(); !strings.Contains(logs, `msg="offline maintenance applied" audit=true op=pstate_clear by=root reason=first`) ||
		strings.Contains(logs, "reason=after") {
		t.Fatalf("applied entries:\n%s", logs)
	}
	if _, ext, _ := (stateHost{b}).Clocks(); !ext.Equal(right) {
		t.Fatalf("the failed reset moved extclock to %v", ext)
	}
	stop()

	// A line this build cannot read stops the journal too.
	if err := os.WriteFile(w.journal(), []byte(`{"v":2,"op":"clock_reset"}`+"\n"+lines[2]), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ = openOnClock(t, w, clk, nil)
	bn = maintenanceBanner(b)
	if bn == nil || bn.Notice.Params()["op"] != MaintenanceClockReset || bn.Notice.Params()["code"] != messages.ErrorBadRequest {
		t.Fatalf("banner of an unreadable entry = %+v", bn)
	}
	if text := w.journalText(); text != `{"v":2,"op":"clock_reset"}`+"\n"+lines[2] {
		t.Fatalf("journal after an unreadable entry = %q", text)
	}
}

// AppendMaintenance writes only entries a boot can apply.
func TestAppendMaintenanceRefusesWhatABootCannotApply(t *testing.T) {
	path := MaintenancePath(t.TempDir())
	at := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	for name, e := range map[string]MaintenanceEntry{
		"version":     {V: 2, Op: MaintenanceClockReset, At: at, By: "root", To: at, Reason: "r"},
		"nobody":      {V: 1, Op: MaintenanceClockReset, At: at, To: at, Reason: "r"},
		"no reason":   {V: 1, Op: MaintenanceClockReset, At: at, By: "root", To: at},
		"no time":     {V: 1, Op: MaintenanceClockReset, At: at, By: "root", Reason: "r"},
		"a grant":     {V: 1, Op: MaintenancePstateClear, At: at, By: "root", Owner: "acme_quota", Kind: extapi.KindAvailability, Reason: "r"},
		"bad id":      {V: 1, Op: MaintenancePstateClear, At: at, By: "root", Owner: "../x", Kind: extapi.KindNodeHolds, Reason: "r"},
		"unknown op":  {V: 1, Op: "suspend", At: at, By: "root", Reason: "r"},
		"mixed shape": {V: 1, Op: MaintenancePstateClear, At: at, By: "root", Owner: "acme_quota", Kind: extapi.KindNodeHolds, To: at, Reason: "r"},
	} {
		if err := AppendMaintenance(path, e); failureCode(err) != messages.ErrorBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused entry created the journal: %v", err)
	}
}
