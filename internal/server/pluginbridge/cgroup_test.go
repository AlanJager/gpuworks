package pluginbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Uninstalling a plugin removes its cgroup directory, also one a previous
// run left behind.
func TestUninstallRemovesThePluginsCgroup(t *testing.T) {
	w := newWorld(t, nil)
	w.cgroupRoot = t.TempDir()
	b, sp := w.openRunning()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	deadline := time.Now().Add(10 * time.Second)
	for len(sp.started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(sp.started()) == 0 {
		t.Fatal("the enabled plugin was never started")
	}
	// The refusing spawner starts nothing; the directory stands for the
	// cgroup the trampoline made and a failed removal left.
	dir := filepath.Join(w.cgroupRoot, "svc", "plugins", "acme_backup")
	other := filepath.Join(w.cgroupRoot, "svc", "plugins", "acme_other")
	for _, d := range []string{dir, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Uninstall(context.Background(), "acme_backup", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the plugin's cgroup outlived its uninstall: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("uninstall removed another plugin's cgroup: %v", err)
	}
}
