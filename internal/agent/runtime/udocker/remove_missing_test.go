package udocker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func removalCLIFixture(t *testing.T, output string) (*Driver, string) {
	t.Helper()
	t.Setenv(launcherEnv, "udocker")
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	for _, dir := range []string{d.stateDir, filepath.Join(d.udockerDir, "containers")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(base, "cli-calls")
	d.udockerExe = writeUdockerStub(t, base, `if [ "$1" = --allow-root ]; then shift; fi
if [ "$1" != rm ] || [ "$#" != 2 ]; then
  echo 'Error: unsupported fixture command' >&2
  exit 64
fi
printf '%s\n' '`+output+`' >&2
exit 1`, calls)
	return d, calls
}

func seedRemovalTracking(t *testing.T, d *Driver, id string) []string {
	t.Helper()
	paths := []string{d.procs().PIDPath(id), d.procs().ExitPath(id), d.LogPath(id), d.SpecPath(id)}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("tracking-evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func assertRemovalTracking(t *testing.T, paths []string, present bool) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if present {
			if err != nil || string(data) != "tracking-evidence" {
				t.Fatalf("failed removal lost tracking %s: %q/%v", path, data, err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("completed removal retained tracking %s: %v", path, err)
		}
	}
}

func TestLegacyRemoveMissingContainerUsesActualInvalidIDFailure(t *testing.T) {
	for _, output := range []string{"Error: invalid container id", "Error: container not found", "Error: no such container"} {
		t.Run(output, func(t *testing.T) {
			d, calls := removalCLIFixture(t, output)
			paths := seedRemovalTracking(t, d, "gpuworks-missing")
			for i := 0; i < 2; i++ {
				if err := d.Remove(context.Background(), "gpuworks-missing"); err != nil {
					t.Fatalf("genuinely absent container is not idempotent: %v", err)
				}
			}
			assertRemovalTracking(t, paths, false)
			data, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(data), "rm gpuworks-missing\n") != 2 {
				t.Fatalf("test bypassed legacy CLI: %q/%v", data, err)
			}
		})
	}
}

func TestLegacyRemoveMissingDoesNotHideOtherCLIFailures(t *testing.T) {
	for _, output := range []string{
		"Error: cannot read container metadata",
		"Error: invalid container id\nError: metadata is corrupt",
		"Error: deleting directory: protected",
	} {
		t.Run(output, func(t *testing.T) {
			d, _ := removalCLIFixture(t, output)
			paths := seedRemovalTracking(t, d, "gpuworks-failure")
			if err := d.Remove(context.Background(), "gpuworks-failure"); err == nil {
				t.Fatal("real CLI failure was reported as absence")
			}
			assertRemovalTracking(t, paths, true)
		})
	}
}

func TestLegacyRemovePreservesNameAliasAndTrackingAcrossRetries(t *testing.T) {
	for _, output := range []string{"Error: invalid container id", "Error: container not found", "Error: deleting container"} {
		t.Run(output, func(t *testing.T) {
			d, _ := removalCLIFixture(t, output)
			realDir := filepath.Join(d.udockerDir, "containers", "a-real-root")
			if err := os.MkdirAll(filepath.Join(realDir, "ROOT"), 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(d.udockerDir, "containers", "gpuworks-alias")
			if err := os.Symlink(filepath.Base(realDir), alias); err != nil {
				t.Fatal(err)
			}
			paths := seedRemovalTracking(t, d, "gpuworks-alias")
			for i := 0; i < 2; i++ {
				if err := d.Remove(context.Background(), "gpuworks-alias"); err == nil {
					t.Fatalf("attempt %d reported an alias rootfs as absent", i)
				}
				if info, err := os.Lstat(alias); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("failure discarded the alias needed by the next retry: %v/%v", info, err)
				}
				if _, err := os.Stat(filepath.Join(realDir, "ROOT")); err != nil {
					t.Fatalf("fallback followed the alias: %v", err)
				}
				assertRemovalTracking(t, paths, true)
			}
		})
	}
}

func TestLegacyRemoveClearsVerifiedDanglingAliasAsAlreadyRemoved(t *testing.T) {
	for _, targetStyle := range []string{"relative", "absolute"} {
		t.Run(targetStyle, func(t *testing.T) {
			d, calls, alias, realDir, paths, otherPaths := aliasRemovalFixture(t)
			if targetStyle == "absolute" {
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realDir, alias); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(realDir); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := d.Remove(context.Background(), "gpuworks-alias"); err != nil {
					t.Fatalf("verified removed rootfs with leftover alias is not idempotent: %v", err)
				}
				if _, err := os.Lstat(alias); !os.IsNotExist(err) {
					t.Fatalf("verified dangling alias remains: %v", err)
				}
				assertRemovalTracking(t, paths, false)
				assertRemovalTracking(t, otherPaths, true)
			}
			if data, err := os.ReadFile(calls); err != nil || strings.Count(string(data), "rm gpuworks-alias\n") != 2 {
				t.Fatalf("dangling cleanup bypassed legacy CLI: %q/%v", data, err)
			}
		})
	}
}

func TestLegacyRemoveRejectsUnsafeNamesBeforeCLIOrStateChanges(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../outside", "/outside", "parent/child", "--all"} {
		t.Run(id, func(t *testing.T) {
			d, calls := removalCLIFixture(t, "Error: invalid container id")
			if err := d.Remove(context.Background(), id); err == nil {
				t.Fatal("unsafe removal ID was accepted")
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatalf("unsafe name reached the CLI: %v", err)
			}
		})
	}
}

func TestLegacyRemoveInspectionErrorCannotBecomeMissing(t *testing.T) {
	d, _ := removalCLIFixture(t, "Error: invalid container id")
	containers := filepath.Join(d.udockerDir, "containers")
	if err := os.Remove(containers); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(containers, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := seedRemovalTracking(t, d, "gpuworks-uninspectable")
	if err := d.Remove(context.Background(), "gpuworks-uninspectable"); err == nil {
		t.Fatal("Lstat inspection failure was swallowed")
	}
	assertRemovalTracking(t, paths, true)
}

func TestLegacyRemoveLstatPermissionFailurePreservesTracking(t *testing.T) {
	for _, phase := range []string{"before-cli", "after-cli"} {
		t.Run(phase, func(t *testing.T) {
			d, calls := removalCLIFixture(t, "Error: invalid container id")
			paths := seedRemovalTracking(t, d, "gpuworks-permission")
			original := removeContainerLstat
			t.Cleanup(func() { removeContainerLstat = original })
			attempts := 0
			removeContainerLstat = func(path string) (os.FileInfo, error) {
				if path == d.containerDir("gpuworks-permission") {
					attempts++
					if phase == "before-cli" || attempts == 2 {
						return nil, &os.PathError{Op: "lstat", Path: path, Err: syscall.EACCES}
					}
				}
				return os.Lstat(path)
			}
			if err := d.Remove(context.Background(), "gpuworks-permission"); !errors.Is(err, syscall.EACCES) {
				t.Fatalf("permission failure became absence: %v", err)
			}
			assertRemovalTracking(t, paths, true)
			if phase == "before-cli" {
				if _, err := os.Stat(calls); !os.IsNotExist(err) {
					t.Fatalf("uninspectable path reached rm: %v", err)
				}
			}
		})
	}
}

func aliasRemovalFixture(t *testing.T) (*Driver, string, string, string, []string, []string) {
	t.Helper()
	d, calls := removalCLIFixture(t, "Error: invalid container id")
	realDir := filepath.Join(d.udockerDir, "containers", "a-real-root")
	otherDir := filepath.Join(d.udockerDir, "containers", "gpuworks-other")
	for _, dir := range []string{realDir, otherDir} {
		if err := os.MkdirAll(filepath.Join(dir, "ROOT"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ROOT", "weights"), []byte("keep payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	alias := d.containerDir("gpuworks-alias")
	if err := os.Symlink(filepath.Base(realDir), alias); err != nil {
		t.Fatal(err)
	}
	return d, calls, alias, realDir, seedRemovalTracking(t, d, "gpuworks-alias"), seedRemovalTracking(t, d, "gpuworks-other")
}

func TestLegacyRemoveRestoresAliasUnlinkedByFailingCLIAcrossRetries(t *testing.T) {
	d, calls, alias, realDir, paths, otherPaths := aliasRemovalFixture(t)
	d.udockerExe = writeUdockerStub(t, filepath.Dir(filepath.Dir(d.udockerExe)),
		`if [ "$1" = --allow-root ]; then shift; fi
if [ "$1" != rm ] || [ "$2" != gpuworks-alias ]; then exit 64; fi
rm -- '`+alias+`'
echo 'Error: invalid container id' >&2
exit 1`, calls)
	for i := 0; i < 2; i++ {
		if err := d.Remove(context.Background(), "gpuworks-alias"); err == nil {
			t.Fatalf("attempt %d forgot a rootfs after the CLI unlinked its alias", i)
		}
		if target, err := os.Readlink(alias); err != nil || target != filepath.Base(realDir) {
			t.Fatalf("original relative alias was not preserved: %q/%v", target, err)
		}
		for _, dir := range []string{realDir, d.containerDir("gpuworks-other")} {
			if payload, err := os.ReadFile(filepath.Join(dir, "ROOT", "weights")); err != nil || string(payload) != "keep payload" {
				t.Fatalf("failed alias removal touched a rootfs: %q/%v", payload, err)
			}
		}
		assertRemovalTracking(t, paths, true)
		assertRemovalTracking(t, otherPaths, true)
	}
}

func TestLegacyRemoveAcceptsNormalCLIAliasAndRootRemoval(t *testing.T) {
	d, calls, alias, realDir, paths, otherPaths := aliasRemovalFixture(t)
	d.udockerExe = writeUdockerStub(t, filepath.Dir(filepath.Dir(d.udockerExe)),
		`if [ "$1" = --allow-root ]; then shift; fi
if [ "$1" != rm ] || [ "$2" != gpuworks-alias ]; then exit 64; fi
rm -rf -- '`+realDir+`'
rm -- '`+alias+`'
exit 0`, calls)
	if err := d.Remove(context.Background(), "gpuworks-alias"); err != nil {
		t.Fatalf("normal CLI alias removal regressed: %v", err)
	}
	for _, path := range []string{alias, realDir} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("normal CLI left its owned path: %s/%v", path, err)
		}
	}
	assertRemovalTracking(t, paths, false)
	assertRemovalTracking(t, otherPaths, true)
	if _, err := os.Stat(filepath.Join(d.containerDir("gpuworks-other"), "ROOT", "weights")); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRemoveRejectsAliasOutsideManagedContainersBeforeCLI(t *testing.T) {
	d, calls := removalCLIFixture(t, "Error: invalid container id")
	outside := t.TempDir()
	alias := d.containerDir("gpuworks-foreign")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	paths := seedRemovalTracking(t, d, "gpuworks-foreign")
	if err := d.Remove(context.Background(), "gpuworks-foreign"); err == nil {
		t.Fatal("foreign alias reached removal")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("unsafe alias reached the CLI: %v", err)
	}
	assertRemovalTracking(t, paths, true)
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRemoveDoesNotReclaimContainerThatAppearedDuringCLI(t *testing.T) {
	d, calls := removalCLIFixture(t, "Error: invalid container id")
	dir := d.containerDir("gpuworks-new-generation")
	d.udockerExe = writeUdockerStub(t, filepath.Dir(filepath.Dir(d.udockerExe)),
		`if [ "$1" = --allow-root ]; then shift; fi
if [ "$1" != rm ] || [ "$2" != gpuworks-new-generation ]; then exit 64; fi
mkdir -p '`+filepath.Join(dir, "ROOT")+`'
echo 'Error: invalid container id' >&2
exit 1`, calls)
	paths := seedRemovalTracking(t, d, "gpuworks-new-generation")
	if err := d.Remove(context.Background(), "gpuworks-new-generation"); err == nil {
		t.Fatal("a newly appeared real rootfs was mistaken for old deletion state")
	}
	if _, err := os.Stat(filepath.Join(dir, "ROOT")); err != nil {
		t.Fatal(err)
	}
	assertRemovalTracking(t, paths, true)
}

func TestLegacyRemoveDoesNotFollowSymlinkedContainersDirectory(t *testing.T) {
	d, calls := removalCLIFixture(t, "Error: invalid container id")
	containers := filepath.Join(d.udockerDir, "containers")
	if err := os.Remove(containers); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "gpuworks-foreign", "ROOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, containers); err != nil {
		t.Fatal(err)
	}
	paths := seedRemovalTracking(t, d, "gpuworks-foreign")
	if err := d.Remove(context.Background(), "gpuworks-foreign"); err == nil {
		t.Fatal("symlinked cache parent escaped the bounded fallback")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("symlinked cache parent reached CLI: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "gpuworks-foreign", "ROOT")); err != nil {
		t.Fatal(err)
	}
	assertRemovalTracking(t, paths, true)
}

func TestLegacyRemovePreservesUnverifiableDanglingAliases(t *testing.T) {
	for _, kind := range []string{"outside-absolute", "outside-relative", "nested-relative", "inside-chain", "outside-chain", "dangling-chain"} {
		t.Run(kind, func(t *testing.T) {
			d, calls := removalCLIFixture(t, "Error: invalid container id")
			containers := filepath.Join(d.udockerDir, "containers")
			outside := t.TempDir()
			target := filepath.Join(outside, "already-absent")
			switch kind {
			case "outside-relative":
				target = "../outside-already-absent"
			case "nested-relative":
				target = "nested/already-absent"
			case "inside-chain", "outside-chain", "dangling-chain":
				target = "target-link"
				real := "inside-root"
				if kind == "inside-chain" {
					if err := os.MkdirAll(filepath.Join(containers, real, "ROOT"), 0o755); err != nil {
						t.Fatal(err)
					}
				} else if kind == "outside-chain" {
					real = outside
				} else {
					real = "missing-chain-root"
				}
				if err := os.Symlink(real, filepath.Join(containers, target)); err != nil {
					t.Fatal(err)
				}
			}
			alias := d.containerDir("gpuworks-unverifiable")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			paths := seedRemovalTracking(t, d, "gpuworks-unverifiable")
			for i := 0; i < 2; i++ {
				if err := d.Remove(context.Background(), "gpuworks-unverifiable"); err == nil {
					t.Fatal("unverifiable outside/chained alias was accepted as missing")
				}
				if got, err := os.Readlink(alias); err != nil || got != target {
					t.Fatalf("unverifiable alias was changed: %q/%v", got, err)
				}
				assertRemovalTracking(t, paths, true)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatalf("unverifiable alias reached CLI removal: %v", err)
			}
		})
	}
}

func TestLegacyRemovePreservesVerifiedDanglingAliasOnRealCLIFailure(t *testing.T) {
	d, calls, alias, realDir, paths, otherPaths := aliasRemovalFixture(t)
	if err := os.RemoveAll(realDir); err != nil {
		t.Fatal(err)
	}
	d.udockerExe = writeUdockerStub(t, filepath.Dir(filepath.Dir(d.udockerExe)),
		`echo 'Error: metadata is corrupt' >&2
exit 1`, calls)
	if err := d.Remove(context.Background(), "gpuworks-alias"); err == nil {
		t.Fatal("dangling alias hid a real CLI failure")
	}
	if _, err := os.Lstat(alias); err != nil {
		t.Fatal(err)
	}
	assertRemovalTracking(t, paths, true)
	assertRemovalTracking(t, otherPaths, true)
}

func TestLegacyRemoveFinishesAliasLeftAfterCLIRemovesRealRoot(t *testing.T) {
	d, calls, alias, realDir, paths, otherPaths := aliasRemovalFixture(t)
	d.udockerExe = writeUdockerStub(t, filepath.Dir(filepath.Dir(d.udockerExe)),
		`if [ "$1" = --allow-root ]; then shift; fi
if [ "$1" != rm ] || [ "$2" != gpuworks-alias ]; then exit 64; fi
rm -rf -- '`+realDir+`'
exit 0`, calls)
	if err := d.Remove(context.Background(), "gpuworks-alias"); err != nil {
		t.Fatalf("CLI removed ROOT but left its name link: %v", err)
	}
	if _, err := os.Lstat(alias); !os.IsNotExist(err) {
		t.Fatalf("unchanged verified dangling alias remains: %v", err)
	}
	assertRemovalTracking(t, paths, false)
	assertRemovalTracking(t, otherPaths, true)
}
