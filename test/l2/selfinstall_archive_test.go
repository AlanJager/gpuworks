//go:build multiproc

package l2

// `gpuworks upgrade --archive` is the upgrade for a host that cannot reach the
// download site: the operator copies the release package over and points the
// installed binary at it. These cases drive it with real packaged builds and
// no site at all, so any attempt to go online would fail the upgrade.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeReleasePackage packs a build the way the download site ships it and
// leaves it where an operator would have copied it.
func writeReleasePackage(t *testing.T, releaseID, binaryPath string) string {
	t.Helper()
	binary := readFileAt(t, binaryPath)
	path := filepath.Join(t.TempDir(), "gpuworks-"+releaseID+".tar.gz")
	if err := os.WriteFile(path, releasePackage(t, releaseID, binary), 0o644); err != nil {
		t.Fatalf("write the release package: %v", err)
	}
	return path
}

func TestUpgradeFromAnArchiveReplacesTheRelease(t *testing.T) {
	archive := writeReleasePackage(t, "v1.2.4", buildServerRelease(t, "v1.2.4"))
	fx := installControlPlane(t, "v1.2.3")

	// The target release is whatever the packaged binary reports.
	check := runTool(t, fx.bin, "upgrade", "--dir", fx.dir, "--archive", archive, "--check")
	if !strings.Contains(check, "update available: v1.2.3 -> v1.2.4") {
		t.Fatalf("check output:\n%s", check)
	}
	if backups, _ := filepath.Glob(filepath.Join(fx.dir, "backups", "*.tar.gz")); len(backups) != 0 {
		t.Fatalf("--check took a snapshot: %v", backups)
	}

	runTool(t, fx.bin, "upgrade", "--dir", fx.dir, "--archive", archive, "--quiet")
	assertHealthVersion(t, fx.base, "v1.2.4")
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.4") {
		t.Fatalf("the installed binary reports %q, want v1.2.4", strings.TrimSpace(got))
	}
	backup := singleBackup(t, fx.dir)
	if !strings.HasPrefix(filepath.Base(backup), "v1.2.3-") {
		t.Fatalf("the snapshot is named %q, want the version it was taken from", filepath.Base(backup))
	}
	entries := readInstalledHistory(t, fx.dir)
	if len(entries) != 2 {
		t.Fatalf("install history = %+v", entries)
	}
	if last := entries[1]; last.Action != "upgrade" || last.Result != "ok" || last.ReleaseID != "v1.2.4" ||
		last.PreviousReleaseID != "v1.2.3" || last.Backup != backup {
		t.Fatalf("the upgrade was not recorded: %+v", last)
	}
	// The package is the operator's; the upgrade leaves it where it was, and
	// nothing it unpacked is left in the install tree.
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("the release package was removed: %v", err)
	}
	if staged, _ := filepath.Glob(filepath.Join(fx.dir, "tmp", "*")); len(staged) != 0 {
		t.Fatalf("the upgrade left %v behind", staged)
	}

	// The same package again is a no-op.
	if out := runTool(t, fx.bin, "upgrade", "--dir", fx.dir, "--archive", archive); !strings.Contains(out, "already at v1.2.4") {
		t.Fatalf("a repeated upgrade printed:\n%s", out)
	}
}

// Without a manifest to trust, the packaged binary is asked what it is before
// anything is stopped; one that cannot answer leaves the service untouched
// rather than being installed and rolled back.
func TestUpgradeFromAnArchiveRefusesABuildThatCannotRun(t *testing.T) {
	archive := writeReleasePackage(t, "v2.0.0", brokenBuild(t))
	fx := installControlPlane(t, "v1.2.3")
	pid := readPidFile(t, filepath.Join(fx.dir, "gpuworks.pid"))

	out, err := runToolErr(t, fx.bin, "upgrade", "--dir", fx.dir, "--archive", archive, "--quiet")
	if err == nil {
		t.Fatalf("an upgrade to a build that cannot run reported success:\n%s", out)
	}
	if !strings.Contains(out, "does not run on this") {
		t.Fatalf("the failure does not say what happened:\n%s", out)
	}
	assertHealthVersion(t, fx.base, "v1.2.3")
	if got := readPidFile(t, filepath.Join(fx.dir, "gpuworks.pid")); got != pid {
		t.Fatalf("the service was restarted (pid %d -> %d)", pid, got)
	}
	if backups, _ := filepath.Glob(filepath.Join(fx.dir, "backups", "*.tar.gz")); len(backups) != 0 {
		t.Fatalf("a refused package took a snapshot: %v", backups)
	}
	if entries := readInstalledHistory(t, fx.dir); len(entries) != 1 {
		t.Fatalf("a refused package was recorded as an upgrade attempt: %+v", entries)
	}
}
