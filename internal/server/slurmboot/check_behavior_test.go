package slurmboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withInterval makes the check script the one setup writes for a cluster whose
// health check interval is n seconds.
func (w *world) withInterval(n string) {
	w.t.Helper()
	path := w.installCheck()
	body := strings.Replace(w.read(path), "\nINTERVAL=300\n", "\nINTERVAL="+n+"\n", 1)
	if !strings.Contains(body, "\nINTERVAL="+n+"\n") {
		w.t.Fatal("the check script has no INTERVAL to set")
	}
	w.writeExec(path, body)
}

// fakeOd answers the random source with a fixed number.
func (w *world) fakeOd(n string) {
	w.t.Helper()
	w.remove("od")
	w.writeExec(filepath.Join(w.bin, "od"), "#!/bin/sh\necho '  "+n+"'\n")
}

// The installer's own exit status and curl's are what the log says, and a
// failed download or install is no "status 0".
func TestCheckSaysWhatBecameOfTheInstaller(t *testing.T) {
	for _, tc := range []struct {
		name, flag string
		want       []string
		notWant    []string
		installer  bool
	}{
		{"download fails", "installer-404", []string{"downloading the installer", "curl status 22", "not installing"}, []string{"status 0", "finished"}, false},
		{"installer fails", "installer-fails", []string{"the installer failed with status 7"}, []string{"status 0", "finished"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.gpuNode()
				w.knownNodes("cn000")
				w.flag(tc.flag, true)
				w.chain(w.program("nhc", 3))
				if r := w.runCheck(); r.code != 3 {
					t.Fatalf("exit %d, want the chained 3", r.code)
				}
				logPath := filepath.Join(w.p.InstallDir, "slurm-check.log")
				w.waitFor("the log", func() bool {
					return strings.Contains(w.read(logPath), tc.want[0])
				})
				time.Sleep(150 * time.Millisecond)
				log := w.read(logPath)
				for _, s := range tc.want {
					if !strings.Contains(log, s) {
						t.Fatalf("log lacks %q:\n%s", s, log)
					}
				}
				for _, s := range tc.notWant {
					if strings.Contains(log, s) {
						t.Fatalf("log has %q:\n%s", s, log)
					}
				}
				if got := len(w.installs()) == 1; got != tc.installer {
					t.Fatalf("installer ran = %v, want %v", got, tc.installer)
				}
			})
		})
	}
}

// The background worker waits a random time (up to half the interval) before
// it asks the server anything; the check slurmd waits for does not wait.
func TestCheckSpreadsTheWorkersAskOverHalfTheInterval(t *testing.T) {
	for _, tc := range []struct {
		od   string // what the random source says
		wait int    // seconds the worker is to wait, interval 4: at most 2
	}{
		{"0", 0}, {"1", 1}, {"7", 1}, {"4294967295", 0}, {"2", 2},
	} {
		t.Run("random "+tc.od, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.gpuNode()
				w.knownNodes("cn000")
				w.env["GWKS_SPREAD_MAX"] = ""
				w.env["FAKE_INSTALL_ACTIVATES"] = "1"
				w.withInterval("4")
				w.fakeOd(tc.od)
				start := time.Now()
				r := w.exec("", filepath.Join(w.p.ScriptDir, "check.sh"))
				if r.code != 0 || r.elapsed > 800*time.Millisecond {
					t.Fatalf("check: code=%d elapsed=%v: it must not wait", r.code, r.elapsed)
				}
				if tc.wait > 0 {
					time.Sleep(300 * time.Millisecond)
					if got := w.stateFile("curl"); got != "" {
						t.Fatalf("the worker asked the server while it was to wait:\n%s", got)
					}
				}
				w.awaitInstall()
				if elapsed := time.Since(start); elapsed < time.Duration(tc.wait)*time.Second {
					t.Fatalf("the install was done after %v, before the wait of %ds", elapsed, tc.wait)
				}
				log := w.read(filepath.Join(w.p.InstallDir, "slurm-check.log"))
				if (tc.wait > 0) != strings.Contains(log, "waiting "+string(rune('0'+tc.wait))+"s (of at most 2s)") {
					t.Fatalf("log = %q (wait %d)", log, tc.wait)
				}
			})
		})
	}
}

// Without od the spread falls back to a checksum, and stays within its bounds.
func TestCheckSpreadFallsBackToAChecksumWithoutOd(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["GWKS_SPREAD_MAX"] = ""
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.withInterval("2")
		w.remove("od")
		w.exec("", filepath.Join(w.p.ScriptDir, "check.sh"))
		w.awaitInstall()
		if n := len(w.installs()); n != 1 {
			t.Fatalf("installs = %d", n)
		}
	})
}

// The original HealthCheckNodeState decides when the chained program runs;
// ours (the install) runs in every state.
func TestCheckChainedProgramKeepsItsOriginalNodeStates(t *testing.T) {
	cases := []struct {
		want, state string
		runs        bool
	}{
		{"", "ALLOCATED", true},
		{"ANY", "ALLOCATED", true},
		{"any", "DOWN*", true},
		{"IDLE", "IDLE", true},
		{"IDLE", "IDLE+DRAIN", true},
		{"IDLE", "IDLE~", true},
		{"IDLE", "ALLOCATED", false},
		{"IDLE", "MIXED", false},
		{"NONDRAINED_IDLE", "IDLE", true},
		{"NONDRAINED_IDLE", "IDLE+DRAIN", false},
		{"NONDRAINED_IDLE", "ALLOCATED", false},
		{"ALLOC", "ALLOCATED", true},
		{"ALLOC", "MIXED", false},
		{"ALLOC", "IDLE", false},
		{"MIXED", "MIXED", true},
		{"MIXED", "ALLOCATED", false},
		{"ALLOC,MIXED", "MIXED", true},
		{"ALLOC,MIXED", "IDLE", false},
		{"IDLE,CYCLE", "IDLE", true},
		{"IDLE,CYCLE", "ALLOCATED", false},
		{"CYCLE", "ALLOCATED", true},
		{"idle", "IDLE", true},
		{"IDLE", "DOWN", false},
		{"IDLE", "DRAINED", true},
	}
	for _, tc := range cases {
		t.Run(tc.want+"/"+tc.state, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.knownNodes("cn000")
				w.write(filepath.Join(w.state, "node-state"), tc.state+"\n")
				prog := w.program("nhc", 4)
				w.write(filepath.Join(w.p.ScriptDir, "chained"), prog+"\n"+tc.want+"\n")
				r := w.runCheck()
				ran := w.exists(prog + ".args")
				if ran != tc.runs {
					t.Fatalf("the original program ran = %v, want %v (exit %d)", ran, tc.runs, r.code)
				}
				if want := map[bool]int{true: 4, false: 0}[tc.runs]; r.code != want {
					t.Fatalf("exit %d, want %d", r.code, want)
				}
			})
		})
	}
}

// A node whose state cannot be read runs the original program (as it did
// before our check stood in front of it), and the install is not held back
// by a state the original program did not want.
func TestCheckChainedStateUnknownRunsAndInstallIsIndependent(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.knownNodes("someone-else") // scontrol does not know this node
		prog := w.program("nhc", 0)
		w.write(filepath.Join(w.p.ScriptDir, "chained"), prog+"\nIDLE\n")
		w.runCheck()
		if !w.exists(prog + ".args") {
			t.Fatal("a node whose state is unknown skipped the original program")
		}
	})
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.write(filepath.Join(w.state, "node-state"), "ALLOCATED\n")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		prog := w.program("nhc", 0)
		w.write(filepath.Join(w.p.ScriptDir, "chained"), prog+"\nIDLE\n")
		w.runCheck()
		w.awaitInstall()
		if w.exists(prog + ".args") {
			t.Fatal("the original program ran on a node in a state it did not ask for")
		}
		if len(w.installs()) != 1 {
			t.Fatal("no install on an allocated node: our own work is not bound to the original node states")
		}
	})
}

// Root must not run anything from, or write through a name in, a directory
// the agent's account owns.
func TestCheckNonRootNeverFollowsLinksInTheAgentsDirectory(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.p.RunAs = "gpuworks"
		w.knownNodes("cn000")
		w.slurmdProc("4242", "/usr/sbin/slurmd", "-D", "-N", "cn000")
		w.flag("agent-active", true) // the order is refreshed on every run, not only before an install
		victim := filepath.Join(w.root, "victim")
		w.write(victim, "precious\n")
		if err := os.MkdirAll(w.p.InstallDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"slurm-gres-devices", "slurm-check.log"} {
			if err := os.Symlink(victim, filepath.Join(w.p.InstallDir, name)); err != nil {
				t.Fatal(err)
			}
		}
		w.runCheck()
		w.runCheck()
		if got := w.read(victim); got != "precious\n" {
			t.Fatalf("root wrote through a link in the agent's directory: %q", got)
		}
		file := filepath.Join(w.p.InstallDir, "slurm-gres-devices")
		if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink != 0 || !strings.Contains(w.read(file), "Gres Name=gpu") {
			t.Fatalf("the card order was not put in place of the link: %v", err)
		}
		if n := strings.Count(w.stateFile("slurmd"), "slurmd -G"); n != 2 {
			t.Fatalf("slurmd -G ran %d times for 2 runs: the order is refreshed on every run", n)
		}
	})
}

// A non-root agent's node logs in a directory root owns.
func TestCheckNonRootLogsOutsideTheAgentsDirectory(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.p.RunAs = "gpuworks"
		w.knownNodes("cn000")
		w.chain(w.program("nhc", 0))
		w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
		w.flag("agent-unit", true)
		w.runCheck()
		if !strings.Contains(w.read(w.path("log/gpuworks-slurm-check.log")), "uninstalling the agent") {
			t.Fatalf("no log under the log directory")
		}
		if w.exists(filepath.Join(w.p.InstallDir, "slurm-check.log")) {
			t.Fatal("root logged inside the agent's directory")
		}
	})
}

// Uninstalling a non-root agent: systemd stops and removes the service, the
// files that make the agent this node are unlinked (a link is removed, not
// followed), and the agent's binary, which that account can replace, never
// runs as root.
func TestCheckNonRootUninstallRunsNothingFromTheAgentsDirectory(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.p.RunAs = "gpuworks"
		w.flag("agent-unit", true)
		w.write(w.path("etc/gpuworks-agent.service"), "[Service]\n")
		w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
		w.writeExec(filepath.Join(w.p.InstallDir, "gpuworks-agent"), "#!/bin/sh\necho \"$*\" >> \""+w.state+"/agent-calls\"\n")
		victim := filepath.Join(w.root, "victim")
		w.write(victim, "precious\n")
		w.write(filepath.Join(w.p.InstallDir, "agent.lock"), "")
		if err := os.Symlink(victim, filepath.Join(w.p.InstallDir, "identity.json")); err != nil {
			t.Fatal(err)
		}
		w.write(filepath.Join(w.p.InstallDir, "gpuworks-agent.conf"), "SLURM_ENROLL_TOKEN=x\n")
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		if got := w.stateFile("agent-calls"); got != "" {
			t.Fatalf("root ran the agent's binary from its own directory: %q", got)
		}
		calls := w.stateFile("systemctl")
		if !strings.Contains(calls, "disable --now gpuworks-agent") || !strings.Contains(calls, "daemon-reload") {
			t.Fatalf("systemctl calls = %q", calls)
		}
		if w.exists(w.path("etc/gpuworks-agent.service")) {
			t.Fatal("the unit file is still there")
		}
		for _, f := range []string{"identity.json", "agent.lock", "gpuworks-agent.conf"} {
			if _, err := os.Lstat(filepath.Join(w.p.InstallDir, f)); err == nil {
				t.Fatalf("%s was not removed", f)
			}
		}
		if w.read(victim) != "precious\n" {
			t.Fatal("the link's target was touched")
		}
		if !w.exists(filepath.Join(w.p.InstallDir, "gpuworks-agent")) {
			t.Fatal("the binary was removed: only the agent's identity goes")
		}
	})
	t.Run("another directory's unit stays", func(t *testing.T) {
		eachShell(t, func(t *testing.T, w *world) {
			w.gpuNode()
			w.p.RunAs = "gpuworks"
			w.flag("agent-unit", true)
			w.env["FAKE_UNIT_STATE"] = "/somewhere/else"
			w.write(w.path("etc/gpuworks-agent.service"), "[Service]\n")
			w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
			w.runCheck()
			if w.stateFile("systemctl") != "" || !w.exists(w.path("etc/gpuworks-agent.service")) {
				t.Fatalf("touched a service that runs from another directory: %q", w.stateFile("systemctl"))
			}
		})
	})
}
