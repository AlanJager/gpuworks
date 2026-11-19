package slurmboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refused runs the setup and wants it refused over the script directory,
// before anything was written there, naming want (when set).
func (w *world) refusedOverScriptDir(t *testing.T, want string, args ...string) map[string]string {
	t.Helper()
	r := w.runSetup(args...)
	if r.code != 1 {
		t.Fatalf("%v: exit %d\n%s", args, r.code, r.stdout)
	}
	if m := marker(t, r.stdout, "precheck"); m["status"] != "failed" || m["reason"] != "script_dir_unsafe" {
		t.Fatalf("%v: precheck = %v\n%s", args, m, r.stdout)
	}
	m := marker(t, r.stdout, "script_dir")
	if m["status"] != "failed" {
		t.Fatalf("%v: script_dir = %v", args, m)
	}
	if want != "" && m["path"] != want {
		t.Fatalf("%v: refused %s, want %s", args, m["path"], want)
	}
	return m
}

// A symbolic link on the path is checked where it leads: the directories
// above its target count, not only those above the link.
func TestSetupChecksWhereASymbolicLinkLeads(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		mustMkdir(t, w.path("data/gwks"), 0o755)
		mustChmod(t, w.path("data"), 0o777)
		mustMkdir(t, w.path("opt"), 0o755)
		if err := os.Symlink(w.path("data/gwks"), w.p.ScriptDir); err != nil {
			t.Fatal(err)
		}
		w.refusedOverScriptDir(t, w.path("data"))
		mustChmod(t, w.path("data"), 0o755)
		if r := w.runSetup(); r.code != 0 {
			t.Fatalf("a link to a safe directory: exit %d\n%s", r.code, r.stdout)
		}
		if !w.exists(w.path("data/gwks/check.sh")) {
			t.Fatal("check.sh not written through the link")
		}
	})
}

// A dangling link is not a missing directory the setup may make.
func TestSetupRefusesADanglingScriptDirectoryLink(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		mustMkdir(t, w.path("opt"), 0o755)
		if err := os.Symlink(w.path("nowhere/gwks"), w.p.ScriptDir); err != nil {
			t.Fatal(err)
		}
		w.refusedOverScriptDir(t, w.p.ScriptDir)
	})
}

// A too open directory holding anything the setup does not leave there (a
// directory, a link, a file others can write) may have been changed by
// someone else: it is refused, not tightened. So is the setup's own when a
// directory above it is unsafe, and then it is left as it was.
func TestSetupDoesNotTightenADirectorySomeoneElseCouldHaveFilled(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, w *world)
		want    func(w *world) string
	}{
		{"a directory inside", func(t *testing.T, w *world) {
			mustMkdir(t, filepath.Join(w.p.ScriptDir, "x"), 0o755)
		}, func(w *world) string { return w.p.ScriptDir }},
		{"a link inside", func(t *testing.T, w *world) {
			if err := os.Symlink("/etc/passwd", filepath.Join(w.p.ScriptDir, ".check.sh.123")); err != nil {
				t.Fatal(err)
			}
		}, func(w *world) string { return w.p.ScriptDir }},
		{"a file others can write", func(t *testing.T, w *world) {
			mustChmod(t, filepath.Join(w.p.ScriptDir, "check.sh"), 0o777)
		}, func(w *world) string { return w.p.ScriptDir }},
		{"an unsafe directory above", func(t *testing.T, w *world) {
			mustChmod(t, w.path("opt"), 0o777)
		}, func(w *world) string { return w.path("opt") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller("nfs", baseConf)
				if r := w.runSetup(); r.code != 0 {
					t.Fatalf("first run: exit %d\n%s", r.code, r.stdout)
				}
				tc.arrange(t, w)
				mustChmod(t, w.p.ScriptDir, 0o777)
				w.refusedOverScriptDir(t, tc.want(w))
				if mode := modeOf(t, w.p.ScriptDir); mode != 0o777 {
					t.Fatalf("a refused run changed the directory to %o", mode)
				}
			})
		})
	}
}

// Uninstalling the agents writes there and copies the token again, so it is
// refused like the setup; removing only takes things away, so it warns and
// goes on.
func TestRemoveWarnsAndUninstallRefusesAnUnsafeScriptDirectory(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		if r := w.runSetup(); r.code != 0 {
			t.Fatalf("setup: exit %d\n%s", r.code, r.stdout)
		}
		mustChmod(t, w.path("opt"), 0o777)
		w.refusedOverScriptDir(t, w.path("opt"), "--uninstall-agents")
		if w.exists(filepath.Join(w.p.ScriptDir, "uninstall-agents")) {
			t.Fatal("a refused uninstall wrote its marker")
		}
		r := w.runSetup("--remove")
		if r.code != 0 {
			t.Fatalf("remove: exit %d\n%s", r.code, r.stdout)
		}
		if m := marker(t, r.stdout, "script_dir"); m["status"] != "warn" || m["path"] != w.path("opt") {
			t.Fatalf("remove script_dir = %v", m)
		}
		if w.read(w.confPath()) != baseConf {
			t.Fatal("remove did not restore slurm.conf")
		}
	})
}

// A node whose script directory another account could change gets nothing,
// the token included, and is listed with why; the other nodes are copied to.
// The same with every copying tool.
func TestSetupCopiesNothingToANodeWithAnUnsafeScriptDirectory(t *testing.T) {
	for _, tools := range [][]string{{"ssh", "scp"}, {"pdsh", "pdcp"}, {"clush"}} {
		t.Run(tools[0], func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller("ext4", baseConf)
				w.onlyRemoteTools(tools...)
				remote := filepath.Join(w.state, "remote", "cn001") + w.p.ScriptDir
				mustMkdir(t, remote, 0o755)
				mustChmod(t, filepath.Dir(remote), 0o777)
				r := w.runSetup()
				byNode := map[string]string{}
				for _, m := range markers(t, r.stdout, "distribute") {
					byNode[m["node"]] = m["status"] + "/" + m["reason"]
				}
				if byNode["cn000"] != "ok/" || byNode["cn001"] != "failed/unsafe" {
					t.Fatalf("per node = %v\n%s", byNode, r.stdout)
				}
				if w.exists(filepath.Join(remote, "token")) || w.exists(filepath.Join(remote, "check.sh")) {
					t.Fatal("files were copied to the unsafe node")
				}
				if !strings.Contains(r.stdout, "is not safe for the health check there") {
					t.Fatalf("no reason in the output:\n%s", r.stdout)
				}
			})
		})
	}
}
