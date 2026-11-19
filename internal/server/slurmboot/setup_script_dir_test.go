package slurmboot

import (
	"os"
	"path/filepath"
	"testing"
)

// slurmd runs check.sh from the script directory as root on every node, so
// a directory another account could write is root on all of them. A
// directory the setup has not made, writable by group or others, is refused
// before anything is changed, with what to do; so is one above it that is,
// unless it is sticky (/tmp), and one not owned by root (a share exported with
// root squash shows root's files as nobody's).
func TestSetupRefusesAScriptDirectoryOthersCanWrite(t *testing.T) {
	cases := []struct {
		name, path string
		arrange    func(w *world)
	}{
		{"the script directory is group writable", "", func(w *world) {
			mustMkdir(t, w.p.ScriptDir, 0o775)
			mustChmod(t, w.p.ScriptDir, 0o775)
		}},
		{"a directory above it is writable by others", "opt", func(w *world) {
			mustMkdir(t, w.path("opt"), 0o777)
			mustChmod(t, w.path("opt"), 0o777)
		}},
		{"it is not root's", "", func(w *world) {
			mustMkdir(t, w.p.ScriptDir, 0o755)
			// stat reports another account as its owner, whoever runs the test
			// (as root in CI, where any directory the test makes is root's)
			w.foreign(w.t, w.p.ScriptDir)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller("nfs", baseConf)
				tc.arrange(w)
				before := w.read(w.confPath())
				for _, args := range [][]string{{"--check"}, nil} {
					r := w.runSetup(args...)
					if r.code != 1 {
						t.Fatalf("%v: exit %d\n%s", args, r.code, r.stdout)
					}
					if m := marker(t, r.stdout, "precheck"); m["status"] != "failed" || m["reason"] != "script_dir_unsafe" {
						t.Fatalf("%v: precheck = %v", args, m)
					}
					m := marker(t, r.stdout, "script_dir")
					if m["status"] != "failed" || m["path"] == "" || m["mode"] == "" {
						t.Fatalf("%v: script_dir = %v", args, m)
					}
					if tc.path != "" && m["path"] != w.path(tc.path) {
						t.Fatalf("%v: refused %s, want %s", args, m["path"], w.path(tc.path))
					}
				}
				if w.read(w.confPath()) != before || len(backups(t, w)) != 0 || w.exists(filepath.Join(w.p.ScriptDir, "check.sh")) {
					t.Fatal("a refused run changed something")
				}
			})
		})
	}
}

// The script directory an earlier setup made, found too open, is the
// setup's own: it is tightened (in check mode only reported); a new one is
// made 0755.
func TestSetupTightensItsOwnScriptDirectoryAndMakesANewOne0755(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		if r := w.runSetup(); r.code != 0 {
			t.Fatalf("first run: exit %d\n%s", r.code, r.stdout)
		}
		if mode := modeOf(t, w.p.ScriptDir); mode != 0o755 {
			t.Fatalf("new script directory mode = %o, want 755", mode)
		}
		mustChmod(t, w.p.ScriptDir, 0o777)
		r := w.runSetup("--check")
		if m := marker(t, r.stdout, "script_dir"); m["status"] != "plan" || m["action"] != "tighten" {
			t.Fatalf("check mode script_dir = %v\n%s", m, r.stdout)
		}
		if mode := modeOf(t, w.p.ScriptDir); mode != 0o777 {
			t.Fatalf("check mode changed the directory to %o", mode)
		}
		r = w.runSetup()
		if r.code != 0 {
			t.Fatalf("second run: exit %d\n%s", r.code, r.stdout)
		}
		if mode := modeOf(t, w.p.ScriptDir); mode&0o022 != 0 {
			t.Fatalf("the setup's own directory is still %o", mode)
		}
	})
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}
