package app

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// A support bundle's server record names the edition next to the version, so
// a supporter knows which build produced it.
func TestDiagnosticsSnapshotNamesTheEdition(t *testing.T) {
	saved := version.Edition
	t.Cleanup(func() { version.Edition = saved })
	version.Edition = "oem-x"
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	docs, err := diagnosticsSources{st: st}.Snapshot(context.Background(), diagnostics.SnapshotScope{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := docs["server"].(map[string]any)
	if server["edition"] != "oem-x" || server["version"] != version.String() {
		t.Fatalf("server record = %+v, want edition oem-x beside the version", server)
	}
}
