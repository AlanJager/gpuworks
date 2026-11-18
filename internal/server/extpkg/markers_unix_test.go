//go:build unix

package extpkg_test

import (
	"bytes"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// While an install or removal holds the plugins directory, recording the
// data marker does not wait for it: it records nothing and says so, and
// records once the directory is free again.
func TestRecordDataMarkerDoesNotWaitForABusyDirectory(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	name := extapi.PluginDataMarker("acme_backup")
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", withState(1))), extpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	unlock, err := h.Dir.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); !errors.Is(err, extpkg.ErrDirBusy) {
		t.Fatalf("record while the directory is held = %v", err)
	}
	if _, ok := mk.markers[name]; ok {
		t.Fatal("a busy directory still recorded the marker")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordDataMarker("acme_backup", 1, "1.0.0"); err != nil || mk.markers[name].Version != 1 {
		t.Fatalf("record once free = %v %v", mk.markers, err)
	}
}
