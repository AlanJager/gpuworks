package extpkg_test

import (
	"bytes"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func withState(version int) func(*extsign.Spec) {
	return func(s *extsign.Spec) {
		s.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: version}
	}
}

// The data marker a healthy run records is max(stored, state_version):
// a build at state version 2 raises it to 2, an older build never lowers
// it, a build without storage writes none, and once it is at 2 a build at
// state version 1 is refused at install and at the check before a start.
func TestRecordDataMarkerNeverLowersIt(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	name := extapi.PluginDataMarker("acme_backup")
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", withState(1))), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mk.markers[name]; ok {
		t.Fatal("install wrote the data marker before the plugin ran")
	}
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordDataMarker("acme_backup", 2, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if got := mk.markers[name]; got != (extapi.Marker{Name: name, Version: 2, WrittenBy: "2.0.0"}) {
		t.Fatalf("marker after version 2 ran = %+v", got)
	}
	sets := mk.sets
	// An older build still running, and a build without storage, leave it.
	for _, v := range []int{1, 2, 0} {
		if err := h.RecordDataMarker("acme_backup", v, "1.0.0"); err != nil {
			t.Fatal(err)
		}
	}
	if got := mk.markers[name]; got.Version != 2 || got.WrittenBy != "2.0.0" || mk.sets != sets {
		t.Fatalf("marker after older runs = %+v (%d writes, want %d)", got, mk.sets, sets)
	}
	_, err := h.Install(bytes.NewReader(pkg(t, w, "0.9.0", withState(1))), extpkg.InstallOptions{})
	if code, params := codeOf(err); code != extapi.ErrorStateTooNew || params["marker"] != name || params["version"] != 2 || params["known"] != 1 || params["needs"] != "2.0.0" {
		t.Fatalf("install of an older build = %v %v", err, params)
	}
	v, err := h.Recheck("acme_backup", false)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := codeOf(h.CheckDataMarker(v.Manifest)); code != extapi.ErrorStateTooNew {
		t.Fatalf("start check of the installed state version 1 build = %v", h.CheckDataMarker(v.Manifest))
	}
	v.Manifest.Storage.StateVersion = 2
	if err := h.CheckDataMarker(v.Manifest); err != nil {
		t.Fatalf("start check at the stored version = %v", err)
	}
}

// A package without storage writes no marker; a package no longer
// installed writes none either, so a purge is not undone; read and write
// failures are returned.
func TestRecordDataMarkerWritesNothingItShouldNot(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	name := extapi.PluginDataMarker("acme_backup")
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); err != nil || len(mk.markers) != 0 {
		t.Fatalf("not installed: %v %v", mk.markers, err)
	}
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordDataMarker("acme_backup", 0, "1.0.0"); err != nil || len(mk.markers) != 0 {
		t.Fatalf("no storage: %v %v", mk.markers, err)
	}
	if err := h.RecordDataMarker("../etc", 1, "1.0.0"); err != nil || len(mk.markers) != 0 {
		t.Fatalf("invalid id: %v %v", mk.markers, err)
	}
	mk.readErr = errors.New("database is locked")
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); !errors.Is(err, mk.readErr) {
		t.Fatalf("read failure = %v", err)
	}
	mk.readErr, mk.setErr = nil, errors.New("disk full")
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); !errors.Is(err, mk.setErr) {
		t.Fatalf("write failure = %v", err)
	}
	mk.setErr = nil
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); err != nil || mk.markers[name].Version != 1 {
		t.Fatalf("retry = %v %v", mk.markers, err)
	}
	if err := h.Remove("acme_backup", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := mk.markers[name]; ok {
		t.Fatal("purge kept the data marker")
	}
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); err != nil || len(mk.markers) != 0 {
		t.Fatalf("after purge: %v %v", mk.markers, err)
	}
}
