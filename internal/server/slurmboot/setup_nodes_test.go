package slurmboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// remoteFile is where a node's copy of a script directory file lives in the
// fake cluster (see the ssh fake).
func (w *world) remoteFile(node, name string) string {
	return filepath.Join(w.state, "remote", node, w.p.ScriptDir, name)
}

// onlyRemoteTools keeps only the named remote-access tools on the PATH.
func (w *world) onlyRemoteTools(keep ...string) {
	w.t.Helper()
	for _, tool := range []string{"ssh", "scp", "pdsh", "pdcp", "clush"} {
		kept := false
		for _, k := range keep {
			kept = kept || k == tool
		}
		if !kept {
			w.remove(tool)
		}
	}
}

func (w *world) calls(tool string) int {
	n := 0
	for _, l := range strings.Split(w.stateFile("calls"), "\n") {
		if strings.HasPrefix(l, tool+" ") {
			n++
		}
	}
	return n
}

func TestSetupSharedScriptDirCopiesNothing(t *testing.T) {
	for _, fs := range []string{"nfs", "nfs4", "lustre", "gpfs", "beegfs", "ceph", "cephfs", "cifs", "smb3", "glusterfs", "fuse.sshfs", "fuse.gpfs", "fuse"} {
		t.Run(fs, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller(fs, baseConf)
				r := w.runSetup()
				if m := marker(t, r.stdout, "shared"); m["status"] != "yes" || m["fstype"] != fs {
					t.Fatalf("shared = %v", m)
				}
				if got := w.stateFile("calls"); strings.Contains(got, "ssh ") || strings.Contains(got, "scp ") || strings.Contains(got, "pdsh ") {
					t.Fatalf("copied although shared:\n%s", got)
				}
				if len(markers(t, r.stdout, "distribute")) != 0 {
					t.Fatal("per-node lines although shared")
				}
				if !strings.Contains(r.stdout, "shared filesystem") || marker(t, r.stdout, "done")["status"] != "ok" {
					t.Fatalf("output:\n%s", r.stdout)
				}
			})
		})
	}
}

func TestSetupLocalScriptDirIsNotShared(t *testing.T) {
	for _, fs := range []string{"ext4", "xfs", "btrfs", "overlay", "tmpfs", "fuseblk"} {
		t.Run(fs, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller(fs, baseConf)
				w.onlyRemoteTools("ssh", "scp")
				r := w.runSetup()
				if m := marker(t, r.stdout, "shared"); m["status"] != "no" || m["fstype"] != fs {
					t.Fatalf("shared = %v", m)
				}
			})
		})
	}
}

func TestSetupFilesystemDetectionFallbacks(t *testing.T) {
	cases := []struct {
		name          string
		findmnt, stat string
		df            string
		wantFS        string
		wantShared    string
	}{
		{"findmnt first", "nfs4", "ext2/ext3", "xfs", "nfs4", "yes"},
		{"stat when findmnt fails", "", "nfs", "xfs", "nfs", "yes"},
		{"df as a last resort", "", "UNKNOWN (0x6969)", "lustre", "lustre", "yes"},
		{"findmnt unknown goes on", "UNKNOWN", "xfs", "nfs", "xfs", "no"},
		{"nothing answers: not shared", "", "UNKNOWN (0x1)", "", "unknown", "no"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller(c.findmnt, baseConf)
				w.onlyRemoteTools("ssh", "scp")
				w.env["FAKE_STAT_FSTYPE"] = c.stat
				w.env["FAKE_DF_FSTYPE"] = c.df
				if c.df == "" {
					w.writeExec(filepath.Join(w.bin, "df"), "#!/bin/sh\nexit 1\n")
				}
				r := w.runSetup()
				if m := marker(t, r.stdout, "shared"); m["status"] != c.wantShared || m["fstype"] != c.wantFS {
					t.Fatalf("shared = %v", m)
				}
			})
		})
	}
}

func TestSetupDistributesWithSSHAndReportsPerNode(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		w.env["FAKE_FAIL_NODES"] = "cn001"
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		for _, n := range []string{"cn000", "cn002"} {
			if got := w.read(w.remoteFile(n, "check.sh")); got != CheckScript(w.p) {
				t.Fatalf("%s: wrong check.sh", n)
			}
			if fi, _ := os.Stat(w.remoteFile(n, "check.sh")); fi.Mode().Perm() != 0o755 {
				t.Fatalf("%s: mode %v", n, fi.Mode().Perm())
			}
			if m := markers(t, r.stdout, "distribute"); len(m) != 3 {
				t.Fatalf("distribute lines = %v", m)
			}
			if !strings.Contains(r.stdout, "  "+n+": OK\n") {
				t.Fatalf("no OK line for %s:\n%s", n, r.stdout)
			}
		}
		if w.exists(w.remoteFile("cn001", "check.sh")) || !strings.Contains(r.stdout, "  cn001: FAILED (unreachable)\n") {
			t.Fatalf("cn001 should have failed:\n%s", r.stdout)
		}
		if w.exists(w.remoteFile("ctl", "check.sh")) || strings.Contains(w.stateFile("calls"), " ctl ") {
			t.Fatal("copied to the controller itself")
		}
		if w.calls("pdsh") != 0 {
			t.Fatal("pdsh used although the PATH has none")
		}
		m := marker(t, r.stdout, "distribute_summary")
		if m["ok"] != "2" || m["failed"] != "cn001" {
			t.Fatalf("summary = %v", m)
		}
		for _, s := range []string{"cn001", w.p.ScriptDir + "/check.sh", "mode 0755"} {
			if !strings.Contains(r.stdout, s) {
				t.Fatalf("manual copy hint lacks %q:\n%s", s, r.stdout)
			}
		}
		if marker(t, r.stdout, "done")["status"] != "partial" {
			t.Fatalf("done = %v", marker(t, r.stdout, "done"))
		}
		// sinfo lists a node once per partition: one attempt per node; two files (check.sh, token) each
		if n := w.calls("scp"); n != 4 {
			t.Fatalf("scp calls = %d, want 4 (cn001 fails before copying; duplicates are listed once)", n)
		}
	})
}

func TestSetupDistributionSendsChainedAndCleansStaleFiles(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf+"HealthCheckProgram=/usr/sbin/nhc\n")
		w.onlyRemoteTools("ssh", "scp")
		// stale flag from an earlier uninstall attempt, on the controller and a node
		w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
		w.write(w.remoteFile("cn000", "uninstall-agents"), "x\n")
		w.runSetup()
		if got := w.read(w.remoteFile("cn000", "chained")); got != "/usr/sbin/nhc\nANY\n" {
			t.Fatalf("remote chained = %q", got)
		}
		if w.exists(w.remoteFile("cn000", "uninstall-agents")) {
			t.Fatal("a stale uninstall flag stayed on a node")
		}
		// the original program disappears from slurm.conf: the stale chained file goes everywhere
		_ = os.WriteFile(w.confPath(), []byte(baseConf), 0o644)
		w.runSetup()
		if w.exists(w.remoteFile("cn000", "chained")) || w.exists(filepath.Join(w.p.ScriptDir, "chained")) {
			t.Fatal("a stale chained file survived")
		}
	})
}

func TestSetupPrefersPdshOverSSH(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp", "pdsh", "pdcp")
		w.env["FAKE_FAIL_NODES"] = "cn002"
		r := w.runSetup()
		if w.calls("pdsh") == 0 || w.calls("pdcp") == 0 || w.calls("ssh") != 0 || w.calls("scp") != 0 {
			t.Fatalf("calls:\n%s", w.stateFile("calls"))
		}
		if !strings.Contains(w.stateFile("calls"), "pdsh -w cn000,cn001,cn002 ") {
			t.Fatalf("one pdsh over the node list expected:\n%s", w.stateFile("calls"))
		}
		for _, n := range []string{"cn000", "cn001"} {
			if w.read(w.remoteFile(n, "check.sh")) != CheckScript(w.p) {
				t.Fatalf("%s: wrong check.sh", n)
			}
		}
		m := marker(t, r.stdout, "distribute_summary")
		if m["ok"] != "2" || m["failed"] != "cn002" || marker(t, r.stdout, "done")["status"] != "partial" {
			t.Fatalf("summary = %v\n%s", m, r.stdout)
		}
		for _, l := range markers(t, r.stdout, "distribute") {
			want := "ok"
			if l["node"] == "cn002" {
				want = "failed"
			}
			if l["status"] != want {
				t.Fatalf("%v, want %s", l, want)
			}
		}
	})
}

func TestSetupUsesClushWhenThereIsNoPdsh(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp", "clush")
		w.env["FAKE_FAIL_NODES"] = "cn000"
		r := w.runSetup()
		if w.calls("clush") == 0 || w.calls("ssh") != 0 {
			t.Fatalf("calls:\n%s", w.stateFile("calls"))
		}
		if w.read(w.remoteFile("cn001", "check.sh")) != CheckScript(w.p) || w.exists(w.remoteFile("cn000", "check.sh")) {
			t.Fatal("wrong files on the nodes")
		}
		if m := marker(t, r.stdout, "distribute_summary"); m["ok"] != "2" || m["failed"] != "cn000" {
			t.Fatalf("summary = %v", m)
		}
	})
}

func TestSetupPdshWithoutPdcpFallsBackToSSH(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp", "pdsh")
		w.runSetup()
		if w.calls("ssh") == 0 || w.calls("pdsh") != 0 {
			t.Fatalf("pdsh cannot copy files by itself:\n%s", w.stateFile("calls"))
		}
	})
}

func TestSetupSSHUsesNonInteractiveOptions(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		w.runSetup()
		for _, l := range strings.Split(w.stateFile("calls"), "\n") {
			if strings.HasPrefix(l, "ssh ") || strings.HasPrefix(l, "scp ") {
				for _, o := range []string{"BatchMode=yes", "ConnectTimeout=10"} {
					if !strings.Contains(l, o) {
						t.Fatalf("%q lacks %s", l, o)
					}
				}
			}
		}
		if !strings.Contains(w.stateFile("calls"), "StrictHostKeyChecking=accept-new") {
			t.Fatal("first contact would prompt")
		}
	})
}

func TestSetupWithNoOtherNodes(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.write(filepath.Join(w.state, "sinfo"), "ctl\nctl\n")
		w.onlyRemoteTools()
		r := w.runSetup()
		if m := marker(t, r.stdout, "distribute_summary"); m["ok"] != "0" || m["failed"] != "none" || marker(t, r.stdout, "done")["status"] != "ok" {
			t.Fatalf("%v\n%s", m, r.stdout)
		}
	})
}

func TestUninstallAgentsDistributesTheFlag(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		w.runSetup()
		w.runSetup("--uninstall-agents")
		if !w.exists(w.remoteFile("cn000", "uninstall-agents")) || !w.exists(w.remoteFile("cn002", "uninstall-agents")) {
			t.Fatal("the flag was not copied to the nodes")
		}
		// re-arming install mode takes the flag off the nodes again
		w.runSetup()
		if w.exists(w.remoteFile("cn000", "uninstall-agents")) {
			t.Fatal("the flag stayed on a node after setup")
		}
	})
}

// The token reaches every node with mode 0600, whatever tool copies it.
func TestSetupDistributesTheTokenRootOnly(t *testing.T) {
	for _, tools := range [][]string{{"ssh", "scp"}, {"pdsh", "pdcp"}, {"clush"}} {
		t.Run(tools[0], func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller("ext4", baseConf)
				w.onlyRemoteTools(tools...)
				r := w.runSetup()
				if r.code != 0 {
					t.Fatalf("exit %d\n%s", r.code, r.stdout)
				}
				for _, n := range []string{"cn000", "cn001", "cn002"} {
					p := w.remoteFile(n, "token")
					if got := w.read(p); got != w.p.Token+"\n" {
						t.Fatalf("%s: token file = %q", n, got)
					}
					if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
						t.Fatalf("%s: token mode = %v, want 0600", n, fi.Mode().Perm())
					}
				}
			})
		})
	}
}
