package pluginbridge

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func withStateVersion(v int) func(*extsign.Spec) {
	return func(s *extsign.Spec) {
		s.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: v}
	}
}

func (w *world) dataMarker(id string) (extapi.Marker, bool) {
	w.t.Helper()
	stored, err := w.st.PluginMarkers()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, m := range stored {
		if m.Name == extapi.PluginDataMarker(id) {
			return m, true
		}
	}
	return extapi.Marker{}, false
}

func codeOfErr(err error) string {
	code, _, _ := codeOf(err)
	return code
}

// The first healthy run of a build with storage records its state version
// in the package's data marker, written by that build's version; starting
// and degraded runs do not, and later healthy runs of the same start do
// not write again. An older build is then refused at install.
func TestTheFirstHealthyRunRecordsTheDataMarker(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "2.0.0", withStateVersion(2)), true)
	if _, ok := w.dataMarker("acme_backup"); ok {
		t.Fatal("install wrote the data marker before the plugin ran")
	}
	for _, s := range []pluginhost.State{pluginhost.StateStarting, pluginhost.StateDegraded, pluginhost.StateBackoff} {
		b.onState(pluginhost.Status{ID: "acme_backup", State: s, Generation: 1})
	}
	if m, ok := w.dataMarker("acme_backup"); ok {
		t.Fatalf("marker before a healthy run = %+v", m)
	}
	b.onState(pluginhost.Status{ID: "acme_backup", State: pluginhost.StateRunning, Generation: 1})
	m, ok := w.dataMarker("acme_backup")
	if !ok || m.Version != 2 || m.WrittenBy != "2.0.0" {
		t.Fatalf("marker after the first healthy run = %+v %v", m, ok)
	}
	// Once recorded for this start, a healthy run after a degraded one
	// reads and writes nothing.
	if err := w.st.DeletePluginMarker(extapi.PluginDataMarker("acme_backup")); err != nil {
		t.Fatal(err)
	}
	b.onState(pluginhost.Status{ID: "acme_backup", State: pluginhost.StateRunning, Generation: 1})
	if _, ok := w.dataMarker("acme_backup"); ok {
		t.Fatal("a later healthy run of the same start wrote the marker again")
	}
	if err := w.st.SetPluginMarker(m); err != nil {
		t.Fatal(err)
	}
	_, err := b.Install(context.Background(), bytes.NewReader(w.pkg("acme_backup", "1.0.0", withStateVersion(1))), InstallRequest{By: "admin"})
	if codeOfErr(err) != extapi.ErrorStateTooNew {
		t.Fatalf("install of a build older than its data = %v", err)
	}
	if v := b.view("acme_backup"); v.Version != "2.0.0" {
		t.Fatalf("refused install replaced the package: %+v", v)
	}
}

// A build without storage writes no data marker, and a build still running
// at an older state version never lowers one a newer build wrote.
func TestOnlyStorageRaisesTheDataMarker(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_off", "1.0.0", nil), true)
	b.onState(pluginhost.Status{ID: "acme_off", State: pluginhost.StateRunning, Generation: 1})
	if m, ok := w.dataMarker("acme_off"); ok {
		t.Fatalf("a package without storage wrote %+v", m)
	}
	install(t, b, w.pkg("acme_backup", "1.0.0", withStateVersion(1)), true)
	newer := extapi.Marker{Name: extapi.PluginDataMarker("acme_backup"), Version: 2, WrittenBy: "v27.1.0"}
	if err := w.st.SetPluginMarker(newer); err != nil {
		t.Fatal(err)
	}
	b.onState(pluginhost.Status{ID: "acme_backup", State: pluginhost.StateRunning, Generation: 2})
	if m, _ := w.dataMarker("acme_backup"); m != newer {
		t.Fatalf("an older running build changed the marker to %+v", m)
	}
	// Both runs went through the recording path: the build without
	// storage is bound at state version 0, the older one is recorded as
	// done for this start without a write.
	b.mu.Lock()
	off, backup := b.pushers["acme_off"], b.pushers["acme_backup"]
	b.mu.Unlock()
	if off.version != "1.0.0" || off.stateVersion != 0 || backup.stateVersion != 1 || !backup.marked {
		t.Fatalf("identities = %+v %+v", off, backup)
	}
}

// A data marker that cannot be read neither starts the build nor leaves it
// refused: the start is tried again, and the plugin starts once the read
// succeeds.
func TestAnUnreadableDataMarkerRetriesTheStart(t *testing.T) {
	w := newWorld(t, nil)
	b, sp := w.openRunning()
	b.markerRetry = 20 * time.Millisecond
	install(t, b, w.pkg("acme_backup", "1.0.0", withStateVersion(1)), false)
	flaky := &flakyMarkers{Markers: b.pkgs.Markers, failures: 2}
	b.pkgs.Markers = flaky
	v, err := b.Enable(context.Background(), "acme_backup", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != string(pluginhost.StateFailed) || v.Code == extapi.ErrorStateTooNew {
		t.Fatalf("view while the marker cannot be read = %+v", v)
	}
	for _, s := range sp.started() {
		if s.ID == "acme_backup" {
			t.Fatal("a build was spawned without its data marker checked")
		}
	}
	if got := waitStarted(t, sp, "1.0.0"); got.ID != "acme_backup" {
		t.Fatalf("started %+v", got)
	}
	if flaky.left() != 0 {
		t.Fatalf("started with %d failures left", flaky.left())
	}
}

// flakyMarkers fails its first reads, then reads through.
type flakyMarkers struct {
	extpkg.Markers
	mu       sync.Mutex
	failures int
}

func (f *flakyMarkers) PluginMarkers() ([]extapi.Marker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("database is locked")
	}
	return f.Markers.PluginMarkers()
}

func (f *flakyMarkers) left() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures
}

// A package whose data is newer than its installed build (a newer build
// ran, then the site went back) is not started: its view is failed with
// PLUGIN_STATE_TOO_NEW until a fit build is installed, enabling it again
// is refused the same way, disabling clears it, and only a purge removes
// the marker.
func TestABuildOlderThanItsDataIsNotStarted(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", withStateVersion(1)), false)
	marker := extapi.Marker{Name: extapi.PluginDataMarker("acme_backup"), Version: 2, WrittenBy: "v27.1.0"}
	if err := w.st.SetPluginMarker(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	b2, sp := w.openRunning()
	v := waitView(t, b2, "acme_backup", func(v View) bool { return v.State == string(pluginhost.StateFailed) })
	if v.Code != extapi.ErrorStateTooNew || v.Params["needs"] != "v27.1.0" || v.Params["known"] != 1 || v.Params["version"] != 2 ||
		len(v.Actions) != 2 || v.Actions[0] != ActionDisable {
		t.Fatalf("view = %+v", v)
	}
	for _, s := range sp.started() {
		if s.ID == "acme_backup" {
			t.Fatal("a build older than its data was spawned")
		}
	}
	if v, err := b2.Disable(context.Background(), "acme_backup", "admin"); err != nil || v.State != StateDisabled || v.Code != "" {
		t.Fatalf("disabled = %+v %v", v, err)
	}
	if v, err := b2.Enable(context.Background(), "acme_backup", "admin"); err != nil || v.State != string(pluginhost.StateFailed) || v.Code != extapi.ErrorStateTooNew {
		t.Fatalf("enabled again = %+v %v", v, err)
	}
	// A build at the data's version starts.
	install(t, b2, w.pkg("acme_backup", "2.0.0", withStateVersion(2)), true)
	if got := waitStarted(t, sp, "2.0.0"); got.ID != "acme_backup" || got.Version != "2.0.0" {
		t.Fatalf("started %+v", got)
	}
	if v := b2.view("acme_backup"); v.State == string(pluginhost.StateFailed) && v.Code == extapi.ErrorStateTooNew {
		t.Fatalf("the refusal outlived the fit build: %+v", v)
	}
	if err := b2.Uninstall(context.Background(), "acme_backup", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.dataMarker("acme_backup"); !ok {
		t.Fatal("uninstall without purge dropped the data marker")
	}
	if len(b2.refusals) != 0 || len(b2.pushers) != 0 {
		t.Fatalf("uninstall left refusals %v pushers %v", b2.refusals, b2.pushers)
	}
	install(t, b2, w.pkg("acme_backup", "2.0.0", withStateVersion(2)), false)
	if err := b2.Uninstall(context.Background(), "acme_backup", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if m, ok := w.dataMarker("acme_backup"); ok {
		t.Fatalf("purge kept the data marker %+v", m)
	}
}

// waitStarted waits until the spawner was asked to start acme_backup at
// version, and returns that spec.
func waitStarted(t *testing.T, sp *refusingSpawner, version string) pluginhost.Spec {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, s := range sp.started() {
			if s.ID == "acme_backup" && s.Version == version {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("acme_backup %s never started; started %d", version, len(sp.started()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
