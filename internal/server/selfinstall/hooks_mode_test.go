package selfinstall

import (
	"context"
	"strings"
	"testing"
)

// unitDirHook is a hook that records the UnitDir it is handed and keeps a
// drop-in, as the server's host identity hook does.
type unitDirHook struct {
	seen []string
}

func (u *unitDirHook) Install(ctx context.Context, run Runner, layout Layout, _ string) error {
	u.seen = append(u.seen, "install "+layout.UnitDir)
	return WriteDropIn(ctx, run, layout, "probe", "[Service]\n")
}

func (u *unitDirHook) Uninstall(ctx context.Context, run Runner, layout Layout) error {
	u.seen = append(u.seen, "uninstall "+layout.UnitDir)
	return RemoveDropIn(ctx, run, layout, "probe")
}

// assertNoUnitStep fails when any command touched a systemd drop-in or ran
// through sudo for one: a detached install by an ordinary account must not
// need privileges for a unit it does not have.
func assertNoUnitStep(t *testing.T, h *harness, hook *unitDirHook) {
	t.Helper()
	for _, seen := range hook.seen {
		if !strings.HasSuffix(seen, " ") {
			t.Errorf("hook saw a unit directory outside systemd: %q", seen)
		}
	}
	for _, cmd := range h.run.executed() {
		if strings.Contains(cmd, ".service.d") {
			t.Errorf("a detached installation ran %q", cmd)
		}
	}
	if len(hook.seen) == 0 {
		t.Error("the hook did not run")
	}
}

// Outside systemd the hooks still run, with no UnitDir, so a drop-in hook
// runs no command: install, upgrade and uninstall of a detached install by
// a non-root account need no sudo for it.
func TestHooksSeeNoUnitDirOutsideSystemd(t *testing.T) {
	t.Run("install", func(t *testing.T) {
		h := newHarness(t, t.TempDir())
		h.systemd = false
		h.installSelf("gpuworks binary v1")
		hook := &unitDirHook{}
		h.installer.WithHooks(hook)
		if report := h.mustInstall(h.quiet()); report.Mode != ModeNohup {
			t.Fatalf("mode = %s", report.Mode)
		}
		assertNoUnitStep(t, h, hook)
	})
	t.Run("upgrade", func(t *testing.T) {
		h := newHarness(t, t.TempDir())
		h.writeExistingInstall("v1.0.0")
		h.supervisedByNohup()
		h.useSite("v2.0.0", []byte("new binary"))
		hook := &unitDirHook{}
		h.installer.WithHooks(hook)
		if _, err := h.installer.Upgrade(context.Background(), h.upgradeReq()); err != nil {
			t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
		}
		assertNoUnitStep(t, h, hook)
		if h.run.ran("sudo systemctl daemon-reload") {
			t.Errorf("a detached upgrade reloaded systemd: %v", h.run.executed())
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		h := uninstallFixture(t)
		h.supervisedByNohup()
		hook := &unitDirHook{}
		h.installer.WithHooks(hook)
		if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err != nil {
			t.Fatal(err)
		}
		assertNoUnitStep(t, h, hook)
	})
}

// Under systemd an upgrade writes a hook's drop-in again, then reloads
// systemd by writing the unit, then restarts: so a drop-in a newer build
// adds is in force when the new binary starts.
func TestUpgradeWritesTheDropInBeforeReloadAndRestart(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))
	hook := &unitDirHook{}
	h.installer.WithHooks(hook)
	if _, err := h.installer.Upgrade(context.Background(), archiveReq(archive)); err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if len(hook.seen) != 1 || hook.seen[0] != "install "+h.layout.UnitDir {
		t.Fatalf("hook saw %q", hook.seen)
	}
	path := h.layout.DropInPath("probe")
	if !h.run.ranInOrder("sudo tee "+path, "sudo systemctl daemon-reload", "sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("upgrade order: %v", h.run.executed())
	}
	if got, ok := h.run.writtenFile(path); !ok || got != "[Service]\n" {
		t.Fatalf("drop-in = %q (%v)", got, ok)
	}
}
