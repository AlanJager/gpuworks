package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Apply the actual filesystem commands, replacing only the service identity
// with this process's own account so the test needs no root or account setup.
type permissionRunner struct{ root string }

func (r permissionRunner) Run(ctx context.Context, cmd Command) error {
	args := slices.Clone(cmd.Args)
	if cmd.Path == "chown" {
		args[1] = strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	}
	scoped := false
	for _, arg := range args {
		if pathWithin(r.root, arg) && filepath.IsAbs(arg) {
			scoped = true
		}
	}
	if !scoped {
		return fmt.Errorf("unscoped permission command: %s", cmd.String())
	}
	output, err := exec.CommandContext(ctx, cmd.Path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", cmd.String(), err, output)
	}
	return nil
}
func (r permissionRunner) Output(_ context.Context, cmd Command) (string, error) {
	if cmd.Path == "id" {
		return strconv.Itoa(os.Getegid()), nil
	}
	return "", fmt.Errorf("unexpected output command: %s", cmd.String())
}

func TestChownTreeSharesExternalDirectories(t *testing.T) {
	root := mustEvalSymlinks(t, t.TempDir())
	layout := NewLayout(filepath.Join(root, "install"), filepath.Join(root, "data"), filepath.Join(root, "logs"))
	for _, dir := range []string{layout.Dir, layout.DataDir, layout.LogsDir} {
		nested := filepath.Join(dir, "nested")
		if err := os.MkdirAll(nested, 0700); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"gpuworks.db", "gpuworks.db-wal", "master.key", "gpuworks-server.log"} {
			if err := os.WriteFile(filepath.Join(nested, file), []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	unrelated := filepath.Join(root, "other-application")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	i := New(layout, Deps{Run: permissionRunner{root}, LookupGroup: credentialGroupFixture}, nil)
	if err := i.chownTree(context.Background(), "fixture-service"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{layout.Dir, layout.DataDir, layout.LogsDir} {
		for _, path := range []string{dir, filepath.Join(dir, "nested")} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0070 != 0070 || info.Mode()&os.ModeSetgid == 0 {
				t.Errorf("directory %s mode = %s, want group rwx and setgid", path, info.Mode())
			}
		}
		for _, file := range []string{"gpuworks.db", "gpuworks.db-wal", "master.key", "gpuworks-server.log"} {
			path := filepath.Join(dir, "nested", file)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0060 != 0060 || info.Mode()&os.ModeSetgid != 0 {
				t.Errorf("file %s mode = %s, want group rw without setgid", path, info.Mode())
			}
			if got := readFile(t, path); got != "existing" {
				t.Errorf("file content changed: %s = %q", path, got)
			}
		}
	}
	info, err := os.Stat(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 || info.Mode()&os.ModeSetgid != 0 {
		t.Errorf("unrelated directory mode changed: %s", info.Mode())
	}
}

func TestChownTreeDeduplicatesCoveredRoots(t *testing.T) {
	for _, kind := range []string{"nested", "same", "external same", "external logs parent", "prefix siblings", "symlink escape", "data parent", "logs parent", "shared parent"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			layout := NewLayout(filepath.Join(root, "install"), "", "")
			want := []string{layout.Dir}
			switch kind {
			case "same":
				layout.DataDir = layout.Dir
				layout.LogsDir = layout.Dir
			case "data parent":
				layout.DataDir = root
				want = []string{root}
			case "logs parent":
				layout.LogsDir = root
				want = []string{root}
			case "shared parent":
				layout.DataDir, layout.LogsDir = root, root
				want = []string{root}
			case "external same":
				layout.DataDir = filepath.Join(root, "external")
				layout.LogsDir = layout.DataDir
				want = append(want, layout.DataDir)
			case "external logs parent":
				layout.LogsDir = filepath.Join(root, "external")
				layout.DataDir = filepath.Join(layout.LogsDir, "data")
				want = append(want, layout.LogsDir)
			case "prefix siblings":
				layout.DataDir = filepath.Join(root, "data")
				layout.LogsDir = filepath.Join(root, "data-old")
				want = append(want, layout.DataDir, layout.LogsDir)
			case "symlink escape":
				target := filepath.Join(root, "external")
				if err := os.MkdirAll(layout.Dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, layout.DataDir); err != nil {
					t.Fatal(err)
				}
				want = append(want, target)
			}
			h := newHarness(t, layout.Dir)
			h.installer.layout = layout
			if err := h.installer.chownTree(context.Background(), "gpuworks"); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, cmd := range h.run.calls {
				if cmd.Path == "chown" {
					got = append(got, cmd.Args[2])
				}
			}
			// Canonical paths are expected on hosts where /tmp itself is a symlink.
			for n, path := range want {
				if resolved, err := resolveUninstallPath(path); err == nil {
					want[n] = resolved
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("permission roots = %v, want %v", got, want)
			}
		})
	}
}

func TestChownTreeReturnsExternalPermissionFailures(t *testing.T) {
	for _, command := range []string{"chown", "chmod", "find"} {
		t.Run(command, func(t *testing.T) {
			h := newHarness(t, filepath.Join(t.TempDir(), "install"))
			h.installer.layout.DataDir = filepath.Join(t.TempDir(), "data")
			failure := errors.New("external permission operation failed")
			dataRoot, err := resolveUninstallPath(h.installer.layout.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			switch command {
			case "chown":
				h.run.fail["sudo chown -R gpuworks:gpuworks "+dataRoot] = failure
			case "chmod":
				h.run.fail["sudo chmod -R g+rwX "+dataRoot] = failure
			case "find":
				h.run.fail["sudo find "+dataRoot] = failure
			}
			if err := h.installer.chownTree(context.Background(), "gpuworks"); !errors.Is(err, failure) {
				t.Fatalf("error = %v, want %v", err, failure)
			}
		})
	}
}

func TestChownTreeRejectsBroadExternalRootsBeforeMutation(t *testing.T) {
	for _, kind := range []string{"filesystem root", "system logs"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			h := newHarness(t, filepath.Join(root, "install"))
			switch kind {
			case "filesystem root":
				h.installer.layout.DataDir = "/"
			case "system logs":
				h.installer.layout.LogsDir = "/var/log"
			}
			err := h.installer.chownTree(context.Background(), "gpuworks")
			if err == nil || !strings.Contains(err.Error(), "permission") {
				t.Fatalf("error = %v, want clear unsafe permission scope error", err)
			}
			if h.run.ran("sudo chown") {
				t.Fatalf("permission mutation started before all roots were checked: %v", h.run.executed())
			}
		})
	}
}

func TestChownTreeSharesSymlinkedExternalData(t *testing.T) {
	root := mustEvalSymlinks(t, t.TempDir())
	layout := NewLayout(filepath.Join(root, "install"), "", "")
	target := filepath.Join(root, "external-data")
	for _, path := range []string{layout.Dir, layout.LogsDir, target} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, layout.DataDir); err != nil {
		t.Fatal(err)
	}
	dataFile := filepath.Join(target, "gpuworks.db")
	if err := os.WriteFile(dataFile, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	i := New(layout, Deps{Run: permissionRunner{root}, LookupGroup: credentialGroupFixture}, nil)
	if err := i.chownTree(context.Background(), "fixture-service"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0070 != 0070 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("symlink target mode = %s, want group rwx and setgid", info.Mode())
	}
	info, err = os.Stat(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0060 != 0060 {
		t.Fatalf("data file mode = %s, want group rw", info.Mode())
	}
}

func TestChownTreeRejectsSymlinkToBroadDirectory(t *testing.T) {
	h := newHarness(t, filepath.Join(t.TempDir(), "install"))
	alias := filepath.Join(t.TempDir(), "shared-root")
	if err := os.Symlink("/", alias); err != nil {
		t.Fatal(err)
	}
	h.installer.layout.DataDir = alias
	if err := h.installer.chownTree(context.Background(), "gpuworks"); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("error = %v, want unsafe permission scope", err)
	}
	if h.run.ran("sudo chown") {
		t.Fatalf("permission mutation started before canonical root was checked: %v", h.run.executed())
	}
}

func TestChownTreeRejectsCanonicalSharedDirectories(t *testing.T) {
	for _, shared := range []string{"/private/var/log", "/private/var/lib", "/private/var/tmp"} {
		for _, linked := range []bool{false, true} {
			for _, field := range []string{"data", "logs"} {
				t.Run(fmt.Sprintf("%s/linked=%t/%s", shared, linked, field), func(t *testing.T) {
					h := newHarness(t, filepath.Join(t.TempDir(), "install"))
					path := shared
					if linked {
						// An existing prefix link resolves even when the canonical
						// macOS directory does not exist on this test host.
						alias := filepath.Join(t.TempDir(), "root-alias")
						if err := os.Symlink("/", alias); err != nil {
							t.Fatal(err)
						}
						path = filepath.Join(alias, strings.TrimPrefix(shared, "/"))
					}
					if field == "data" {
						h.installer.layout.DataDir = path
					} else {
						h.installer.layout.LogsDir = path
					}
					err := h.installer.chownTree(context.Background(), "gpuworks")
					if err == nil || !strings.Contains(err.Error(), "broad directory") {
						t.Fatalf("error = %v, want shared directory rejection", err)
					}
					if h.run.ran("sudo chown") || h.run.ran("sudo chmod") || h.run.ran("sudo find") {
						t.Fatalf("permissions changed before all shared roots were checked: %v", h.run.executed())
					}
				})
			}
		}
	}
}

func TestChownTreeRejectsCanonicalSystemRootTargets(t *testing.T) {
	for _, root := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		t.Run(root, func(t *testing.T) {
			target, err := filepath.EvalSymlinks(root)
			if errors.Is(err, os.ErrNotExist) {
				t.Logf("system root %s is absent on this host", root)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, filepath.Join(t.TempDir(), "install"))
			h.installer.layout.DataDir = target
			err = h.installer.chownTree(context.Background(), "gpuworks")
			if err == nil || !strings.Contains(err.Error(), "broad directory") {
				t.Fatalf("canonical system root %s -> %s: error = %v", root, target, err)
			}
			if h.run.ran("sudo chown") || h.run.ran("sudo chmod") || h.run.ran("sudo find") {
				t.Fatalf("canonical system root caused permission changes: %v", h.run.executed())
			}
		})
	}
}

func TestChownTreeRejectsCanonicalHomeTarget(t *testing.T) {
	root := mustEvalSymlinks(t, t.TempDir())
	homeDir := filepath.Join(root, "home")
	if err := os.Mkdir(homeDir, 0700); err != nil {
		t.Fatal(err)
	}
	homeAlias := filepath.Join(root, "home-alias")
	if err := os.Symlink(homeDir, homeAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeAlias)
	if err := checkDedicatedDirectory(filepath.Join(homeDir, "gpuworks")); err != nil {
		t.Fatalf("a dedicated directory under HOME must remain valid: %v", err)
	}
	h := newHarness(t, filepath.Join(root, "install"))
	h.installer.layout.LogsDir = homeDir
	err := h.installer.chownTree(context.Background(), "gpuworks")
	if err == nil || !strings.Contains(err.Error(), "broad directory") {
		t.Fatalf("canonical HOME target accepted: %v", err)
	}
	if h.run.ran("sudo chown") || h.run.ran("sudo chmod") || h.run.ran("sudo find") {
		t.Fatalf("canonical HOME target caused permission changes: %v", h.run.executed())
	}
}

func TestDedicatedDirectoryRejectsOtherUserHomes(t *testing.T) {
	for _, root := range []string{"/home", "/Users"} {
		paths := []string{filepath.Join(root, "other-user")}
		if canonical, err := filepath.EvalSymlinks(root); err == nil {
			paths = append(paths, filepath.Join(canonical, "other-user"))
			alias := filepath.Join(t.TempDir(), "home-root-alias")
			if err := os.Symlink(canonical, alias); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, filepath.Join(alias, "other-user"))
		}
		for _, path := range paths {
			if err := checkDedicatedDirectory(path); err == nil || !strings.Contains(err.Error(), "user home directory") {
				t.Errorf("user home directory %s accepted: %v", path, err)
			}
			if err := checkDedicatedDirectory(filepath.Join(path, "gpuworks")); err != nil {
				t.Errorf("a dedicated application directory under %s must remain valid: %v", path, err)
			}
		}
	}
}

func TestInstallAllowsDedicatedParentDataOrLogs(t *testing.T) {
	for _, parentField := range []string{"data", "logs", "both"} {
		t.Run(parentField, func(t *testing.T) {
			root := mustEvalSymlinks(t, t.TempDir())
			appDir := filepath.Join(root, "gpuworks")
			layout := NewLayout(filepath.Join(appDir, "server"), "", "")
			if parentField != "logs" {
				layout.DataDir = appDir
			}
			if parentField != "data" {
				layout.LogsDir = appDir
			}
			layout.UnitDir = filepath.Join(root, "units")
			layout.LaunchdDir = filepath.Join(root, "launchd")
			if err := os.MkdirAll(appDir, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(appDir, "existing-application-file")
			writeFile(t, marker, "keep existing data", 0600)
			outside := filepath.Join(root, "other-application-file")
			writeFile(t, outside, "unrelated", 0600)

			h := newHarness(t, layout.Dir)
			h.layout = layout
			h.users["fixture-service"] = true
			realPermissions := permissionRunner{root}
			h.run.on = func(cmd Command) {
				switch cmd.Path {
				case "chown", "chmod", "find":
					if err := realPermissions.Run(t.Context(), cmd); err != nil {
						t.Fatal(err)
					}
				case "systemctl":
					if len(cmd.Args) > 0 && cmd.Args[0] == "restart" {
						for _, dir := range []string{layout.Dir, layout.DataDir, layout.LogsDir} {
							info, err := os.Stat(dir)
							if err != nil {
								t.Fatal(err)
							}
							if info.Mode().Perm()&0070 != 0070 || info.Mode()&os.ModeSetgid == 0 {
								t.Fatalf("service started before directory was shared: %s mode=%s", dir, info.Mode())
							}
						}
					}
				}
			}
			h.installer = New(h.layout, h.deps, h.io)
			h.installSelf("installed fixture binary")
			req := h.quiet()
			req.ServiceUser = "fixture-service"
			req.Password = "permission-regression-password-2026"
			req.DataDirSet, req.LogsDirSet = true, true
			report := h.mustInstall(req)
			if report.Layout.DataDir != layout.DataDir || report.Layout.LogsDir != layout.LogsDir {
				t.Fatalf("parent layout changed: %+v", report.Layout)
			}
			info, err := os.Stat(marker)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0060 != 0060 || readFile(t, marker) != "keep existing data" {
				t.Fatal("existing application file was not preserved and shared")
			}
			info, err = os.Stat(outside)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 || readFile(t, outside) != "unrelated" {
				t.Fatal("permission changes escaped the selected application root")
			}
			var chownRoots []string
			for _, cmd := range h.run.calls {
				if cmd.Path == "chown" {
					chownRoots = append(chownRoots, cmd.Args[2])
				}
			}
			if !slices.Equal(chownRoots, []string{appDir, appDir}) {
				t.Fatalf("permission roots = %v, want the selected parent before start and at completion", chownRoots)
			}
		})
	}
}
