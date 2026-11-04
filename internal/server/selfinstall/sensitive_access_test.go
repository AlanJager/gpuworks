package selfinstall

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// denyProtect makes every descriptor-based protection fail the way it does for
// an operator whose shell has not yet picked up the service group, or for a
// service account reading a root-owned snapshot.
// Paths listed in only are denied; with none, every path is.
func denyProtect(t *testing.T, only ...string) {
	t.Helper()
	previous := protectFile
	protectFile = func(path string, policy securefile.Policy) error {
		if len(only) > 0 && !slices.Contains(only, path) {
			return previous(path, policy)
		}
		return &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	t.Cleanup(func() { protectFile = previous })
}

func writeSensitive(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("existing-sensitive"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func sensitiveLayout(t *testing.T) Layout {
	t.Helper()
	layout := NewLayout(filepath.Join(mustEvalSymlinks(t, t.TempDir()), "install"), "", "")
	for _, dir := range []string{layout.Dir, layout.DataDir, layout.LogsDir, layout.BackupsDir} {
		if err := os.MkdirAll(dir, 0775); err != nil {
			t.Fatal(err)
		}
	}
	return layout
}

func TestChownTreeAcceptsHandedOverFilesTheOperatorCannotOpen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("group sharing is Linux-only")
	}
	layout := sensitiveLayout(t)
	paths := []string{
		filepath.Join(layout.DataDir, "master.key"),
		filepath.Join(layout.DataDir, "initial-credentials"),
		layout.BackupPath("v1", NewDeps(nil).now()),
	}
	for _, path := range paths {
		writeSensitive(t, path, 0600)
	}
	denyProtect(t)
	i := New(layout, Deps{Run: permissionRunner{filepath.Dir(layout.Dir)}, LookupGroup: func(name string) (*user.Group, error) {
		return &user.Group{Name: name, Gid: strconv.Itoa(os.Getgid())}, nil
	}}, nil)
	if err := i.chownTree(context.Background(), "fixture-service"); err != nil {
		t.Fatalf("compliant handed-over files rejected: %v", err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != securefile.GroupMode {
			t.Errorf("%s mode=%#o, want %#o", path, info.Mode().Perm(), securefile.GroupMode)
		}
	}
}

func TestChownTreeRootServiceAcceptsPrivateFilesTheOperatorCannotOpen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the root service branch is Linux-only")
	}
	layout := sensitiveLayout(t)
	writeSensitive(t, filepath.Join(layout.DataDir, "initial-credentials"), 0600)
	denyProtect(t)
	i := New(layout, Deps{Run: permissionRunner{filepath.Dir(layout.Dir)}}, nil)
	if err := i.chownTree(context.Background(), "root"); err != nil {
		t.Fatalf("private root-owned credentials rejected: %v", err)
	}
}

func TestUnopenableSensitiveFileMustAlreadyMatchPolicy(t *testing.T) {
	layout := sensitiveLayout(t)
	exposed := filepath.Join(layout.DataDir, "master.key")
	writeSensitive(t, exposed, 0664)
	denyProtect(t)
	err := protectHandedOverFile(exposed, securefile.Policy{})
	if err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("exposed key accepted without protection: %v", err)
	}
	target := filepath.Join(layout.Dir, "elsewhere")
	writeSensitive(t, target, 0600)
	link := filepath.Join(layout.DataDir, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := protectHandedOverFile(link, securefile.Policy{}); err == nil {
		t.Fatal("verification followed a symlink")
	}
}

func TestStartupHardeningWarnsAboutSnapshotsItCannotProtect(t *testing.T) {
	layout := sensitiveLayout(t)
	writeSensitive(t, filepath.Join(layout.DataDir, "master.key"), 0600)
	backup := layout.BackupPath("v1", NewDeps(nil).now())
	writeSensitive(t, backup, 0664)
	denyProtect(t, backup)

	var warned []string
	warn := func(path string, err error) {
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("warning lost the permission cause: %v", err)
		}
		warned = append(warned, path)
	}
	if err := HardenSensitiveFiles(layout, warn); err != nil {
		t.Fatalf("unprotectable snapshot blocked startup: %v", err)
	}
	if len(warned) != 1 || warned[0] != backup {
		t.Fatalf("warned=%v, want only %s", warned, backup)
	}
	if err := New(layout, Deps{}, nil).hardenSensitiveFiles(context.Background(), securefile.Policy{}); err == nil {
		t.Fatal("installer-side hardening must still refuse an exposed snapshot")
	}

	key := filepath.Join(layout.DataDir, "master.key")
	writeSensitive(t, key, 0664)
	denyProtect(t, key)
	warned = nil
	if err := HardenSensitiveFiles(layout, warn); err == nil {
		t.Fatal("exposed credential was downgraded to a warning")
	}
}

// The upgrade preflight and the backup writer go on to read these files, so
// they must refuse up front, before the service is stopped, instead of
// accepting a compliant file this process cannot open.
func TestPreflightAndBackupRefuseCompliantFilesTheyCannotOpen(t *testing.T) {
	layout := sensitiveLayout(t)
	writeSensitive(t, filepath.Join(layout.DataDir, "master.key"), 0600)
	denyProtect(t)
	i := New(layout, Deps{}, nil)
	err := i.preflightSensitiveFiles(context.Background())
	if err == nil || !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "retry install or upgrade") {
		t.Fatalf("preflight accepted an unreadable credential: %v", err)
	}
	if err := hardenFilesWithPolicy(layout, securefile.Policy{}); err == nil {
		t.Fatal("backup writer accepted an unreadable credential")
	}
}
