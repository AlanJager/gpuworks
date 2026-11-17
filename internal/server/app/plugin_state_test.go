package app

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A server older than the plugin state a newer one persisted refuses to
// start, naming the version to run, and leaves the database as it was.
func TestNewRefusesNewerPluginState(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "gpuworks.db")
	st, err := store.Open(db, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	marker := extapi.Marker{Name: "pstate.availability", Version: 3, WrittenBy: "v27.4.0"}
	if err := st.SetPluginMarker(marker); err != nil {
		t.Fatal(err)
	}
	st.Close()

	cfg := Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: db, AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026"}
	_, err = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var xe *extapi.Error
	if extapi.CodeOf(err) != extapi.ErrorStateTooNew {
		t.Fatalf("New = %v, want %s", err, extapi.ErrorStateTooNew)
	}
	if e, ok := err.(*extapi.Error); ok {
		xe = e
	}
	if xe == nil || xe.Params["needs"] != "v27.4.0" {
		t.Fatalf("refusal does not name the version to run: %#v", err)
	}
	// The refusal closed the database and changed nothing in it.
	got, err := store.ReadPluginMarkers(db)
	if err != nil || len(got) != 1 || got[0] != marker {
		t.Fatalf("markers after the refusal = %v %v", got, err)
	}
	st, err = store.Open(db, bus.New())
	if err != nil {
		t.Fatalf("database still held after the refusal: %v", err)
	}
	if err := st.DeletePluginMarker(marker.Name); err != nil {
		t.Fatal(err)
	}
	st.Close()
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New without newer state = %v", err)
	}
	a.proxy.Close()
	a.store.Close()
}
