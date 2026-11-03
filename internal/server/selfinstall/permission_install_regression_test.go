package selfinstall

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Selecting the historical application root for installation must not inherit
// the extra restriction on deleting that root during uninstall.
func TestPermissionRootsAcceptHistoricalApplicationRoot(t *testing.T) {
	want, err := resolveUninstallPath("/opt/gpuworks")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"install", "data", "logs"} {
		t.Run(role, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			switch role {
			case "install":
				h.installer.layout = NewLayout("/opt/gpuworks", "", "")
			case "data":
				h.installer.layout.DataDir = "/opt/gpuworks"
			case "logs":
				h.installer.layout.LogsDir = "/opt/gpuworks"
			}
			roots, err := h.installer.permissionRoots()
			if err != nil {
				t.Fatalf("historical %s root rejected: %v", role, err)
			}
			if !slices.Contains(roots, want) {
				t.Fatalf("permission roots = %v, want %s", roots, want)
			}
			if role == "install" && len(roots) != 1 {
				t.Fatalf("nested roots were not deduplicated: %v", roots)
			}
		})
	}
	if err := safeUninstallDirectory("/opt/gpuworks"); err == nil {
		t.Fatal("install compatibility must not allow deleting the shared agent/server root")
	}
}

func TestInstallPermissionRejectionPreservesExistingInstallation(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.installSelf("replacement binary")
	// The filesystem is valid, but the installation root is deliberately a
	// protected HOME identity. A deterministic policy rejection must precede
	// binary/config/unit writes or stopping an existing service.
	t.Setenv("HOME", h.layout.Dir)
	paths := []string{h.layout.Binary(), h.layout.ConfigPath(), h.layout.UnitPath(),
		filepath.Join(h.layout.DataDir, "gpuworks.db"), filepath.Join(h.layout.DataDir, "master.key")}
	before := map[string][]byte{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	if _, err := h.installer.Install(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "refusing recursive permission changes for "+h.layout.Dir) {
		t.Fatalf("want permission-policy rejection, got %v", err)
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(before[path], after) {
			t.Errorf("installation was modified before permission rejection: %s", path)
		}
	}
	for _, action := range []string{"stop", "restart", "start", "daemon-reload"} {
		if h.run.ran("sudo systemctl " + action) {
			t.Errorf("service action %s ran before permission rejection", action)
		}
	}
}

func TestInstallPermissionRejectionCreatesNoPartialInstallation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	h := newHarness(t, dir)
	h.installSelf("new binary")
	t.Setenv("HOME", h.layout.Dir)
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "password"
	if _, err := h.installer.Install(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "refusing recursive permission changes") {
		t.Fatalf("want permission-policy rejection, got %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("rejected install created its directory: %v", err)
	}
}
