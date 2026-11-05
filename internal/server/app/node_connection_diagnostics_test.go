package app

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func TestNodeConnectionDiagnosticsExposureIsHistoricalAndOptional(t *testing.T) {
	dir := t.TempDir()
	i := connectionInstaller(provision.NewSSHInstaller(nil), dir)
	if got := i.ConnectionExposure(); got != nil {
		t.Fatalf("missing connector must remain unknown: %+v", got)
	}
	at := time.Date(2026, 10, 23, 10, 19, 0, 0, time.UTC)
	raw, _ := json.Marshal(&expose.Status{URL: "https://last-observed.example", State: "ready", UpdatedAt: at})
	if err := i.exposure.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.exposure.StatusPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := i.ConnectionExposure()
	if got == nil || got.URL != "https://last-observed.example" || !got.ObservedAt.Equal(at) {
		t.Fatalf("last observation changed: %+v", got)
	}
	if err := os.WriteFile(i.exposure.StatusPath(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := i.ConnectionExposure(); got != nil {
		t.Fatalf("corrupt connector snapshot must remain unknown: %+v", got)
	}
	if err := i.exposure.WriteStatus(&expose.Status{URL: "https://user:secret@host?key=value", State: "ready", UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if got := i.ConnectionExposure(); got != nil {
		t.Fatal("unsafe connector address was exposed")
	}
}
