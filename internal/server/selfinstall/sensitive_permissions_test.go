package selfinstall

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/securefile"
)

func TestChownTreeProtectsSensitiveFilesAndExistingBackups(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "external"}[external], func(t *testing.T) {
			root := mustEvalSymlinks(t, t.TempDir())
			layout := NewLayout(filepath.Join(root, "install"), "", "")
			if external {
				layout = NewLayout(layout.Dir, filepath.Join(root, "external-data"), filepath.Join(root, "external-logs"))
			}
			for _, dir := range []string{layout.Dir, layout.DataDir, layout.LogsDir, layout.BackupsDir} {
				if err := os.MkdirAll(dir, 0775); err != nil {
					t.Fatal(err)
				}
			}
			names := []string{"master.key", "gpuworks.db", "gpuworks.db-wal", "gpuworks.db-shm", "initial-credentials"}
			var paths []string
			for _, name := range names {
				paths = append(paths, filepath.Join(layout.DataDir, name))
			}
			paths = append(paths, layout.BackupPath("v1", NewDeps(nil).now()))
			for _, path := range paths {
				if err := os.WriteFile(path, []byte("existing-sensitive"), 0664); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0664); err != nil {
					t.Fatal(err)
				}
			}
			i := New(layout, Deps{Run: permissionRunner{root}, LookupGroup: func(name string) (*user.Group, error) {
				return &user.Group{Name: name, Gid: strconv.Itoa(os.Getgid())}, nil
			}}, nil)
			if err := i.chownTree(context.Background(), "fixture-service"); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				wantMode := securefile.OwnerMode
				if runtime.GOOS == "linux" {
					wantMode = securefile.GroupMode
				}
				if info.Mode().Perm() != wantMode {
					t.Errorf("%s mode=%#o, want0660", path, info.Mode().Perm())
				}
				if got := readFile(t, path); got != "existing-sensitive" {
					t.Fatalf("contents changed: %s", path)
				}
			}
		})
	}
}

func TestSensitivePolicyRequiresBoundExplicitAuthorizedGroup(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(filepath.Join(root, "install"), "", "")
	layout.UnitDir = filepath.Join(root, "units")
	if err := os.MkdirAll(layout.UnitDir, 0755); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (*user.Group, error) {
		if name != DefaultCredentialGroup {
			t.Fatalf("looked up incidental group %s", name)
		}
		return &user.Group{Name: name, Gid: strconv.Itoa(os.Getgid())}, nil
	}
	for _, tt := range []struct {
		name, user, group, binary, goos string
		trusted                         bool
	}{
		{"explicit", "custom-service", DefaultCredentialGroup, layout.Binary(), "linux", true},
		{"legacy reserved user", DefaultServiceUser, "", layout.Binary(), "linux", true},
		{"ordinary primary group", "nobody", "", layout.Binary(), "linux", false},
		{"ordinary explicit group", "custom-service", "nogroup", layout.Binary(), "linux", false},
		{"root", "root", DefaultCredentialGroup, layout.Binary(), "linux", false},
		{"another install", "custom-service", DefaultCredentialGroup, filepath.Join(root, "other", "gpuworks"), "linux", false},
		{"macOS staff", "operator", "staff", layout.Binary(), "darwin", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unit := "[Service]\nUser=" + tt.user + "\nGroup=" + tt.group + "\nExecStart=" + tt.binary + " serve\n"
			if err := os.WriteFile(layout.UnitPath(), []byte(unit), 0644); err != nil {
				t.Fatal(err)
			}
			policy, err := sensitiveFilePolicy(layout, tt.goos, lookup)
			if err != nil {
				t.Fatal(err)
			}
			gid, group := policy.GroupID()
			if group != tt.trusted || group && gid != os.Getgid() {
				t.Fatalf("policy=%v/%d trusted=%v", group, gid, tt.trusted)
			}
		})
	}
}

func TestSensitivePolicyLegacyMissingGroupStaysOwnerOnly(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(filepath.Join(root, "install"), "", "")
	layout.UnitDir = filepath.Join(root, "units")
	if err := os.MkdirAll(layout.UnitDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, group string
		lookupErr   error
		wantErr     bool
	}{
		{"legacy missing", "", user.UnknownGroupError(DefaultCredentialGroup), false},
		{"explicit missing", DefaultCredentialGroup, user.UnknownGroupError(DefaultCredentialGroup), true},
		{"legacy lookup failure", "", errors.New("group database unavailable"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unit := "[Service]\nUser=" + DefaultServiceUser + "\nGroup=" + tt.group + "\nExecStart=" + layout.Binary() + " serve\n"
			if err := os.WriteFile(layout.UnitPath(), []byte(unit), 0644); err != nil {
				t.Fatal(err)
			}
			policy, err := sensitiveFilePolicy(layout, "linux", func(string) (*user.Group, error) {
				return nil, tt.lookupErr
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("policy error=%v, want error=%v", err, tt.wantErr)
			}
			if policy.Mode() != securefile.OwnerMode {
				t.Fatal("unresolved group granted access")
			}
		})
	}
}

func TestHardenSensitiveFilesRejectsLinksAndLeavesUnrelatedFilesAlone(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(filepath.Join(root, "install"), "", "")
	if err := os.MkdirAll(layout.DataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.BackupsDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "unrelated")
	if err := os.WriteFile(target, []byte("not a key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(layout.DataDir, "master.key")); err != nil {
		t.Fatal(err)
	}
	if err := HardenSensitiveFiles(layout, nil); err == nil {
		t.Fatal("linked key accepted")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatal("outside target permissions changed")
	}
	os.Remove(filepath.Join(layout.DataDir, "master.key"))
	note := filepath.Join(layout.BackupsDir, "README.txt")
	if err := os.WriteFile(note, []byte("public"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := HardenSensitiveFiles(layout, nil); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(note)
	if info.Mode().Perm() != 0644 {
		t.Fatal("unrelated backup-directory file was restricted")
	}
	path := layout.BackupPath("v1", NewDeps(nil).now())
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := HardenSensitiveFiles(layout, nil); err == nil {
		t.Fatal("linked snapshot accepted")
	}
}

func TestRestoreLegacyPublicDataMembersUsesCurrentPrivateMode(t *testing.T) {
	h := newHarness(t, t.TempDir())
	archive := writeArchive(t, h.layout.Dir, "legacy.tar.gz", map[string]string{
		backupDataPrefix + "master.key": "original-key", backupDataPrefix + "gpuworks.db": "original-db",
	})
	if err := h.installer.restoreBackup(archive); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"master.key", "gpuworks.db"} {
		path := filepath.Join(h.layout.DataDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("legacy member %s restored public: %#o", name, info.Mode().Perm())
		}
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "master.key")); got != "original-key" {
		t.Fatal("legacy key rotated")
	}
}

func TestInstallFinalPermissionFailureCannotReportSuccess(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.ServiceUser = "gpuworks"
	h.run.on = func(cmd Command) {
		if cmd.String() == "sudo systemctl restart gpuworks" {
			writeFile(t, filepath.Join(h.layout.DataDir, "master.key"), "sensitive", 0664)
			h.run.fail["sudo chown -R gpuworks:gpuworks "+h.layout.Dir] = errors.New("final grant rejected")
		}
	}
	_, err := h.installer.Install(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "protect completed installation") {
		t.Fatalf("install error=%v", err)
	}
	if entries, err := ReadHistory(h.layout.HistoryPath()); err == nil {
		for _, entry := range entries {
			if entry.Result == "ok" {
				t.Fatal("history recorded success after permission failure")
			}
		}
	}
}
