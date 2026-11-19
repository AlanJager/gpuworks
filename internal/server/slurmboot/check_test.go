package slurmboot

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gpuNode makes the machine look like a GPU node (device node and nvidia-smi).
func (w *world) gpuNode() {
	w.t.Helper()
	w.write(w.path("dev/nvidia0"), "")
	w.flag("gpu", true)
}

// chain writes the chained file naming prog.
func (w *world) chain(prog string) {
	w.t.Helper()
	w.write(filepath.Join(w.p.ScriptDir, "chained"), prog+"\n")
}

func (w *world) knownNodes(names ...string) {
	w.write(filepath.Join(w.state, "nodes"), strings.Join(names, "\n")+"\n")
}

// awaitInstall waits for the background installer to finish.
func (w *world) awaitInstall() {
	w.t.Helper()
	w.waitFor("the installer to finish", func() bool { return w.exists(filepath.Join(w.state, "installer-done")) })
	// let the worker release its lock and exit
	time.Sleep(150 * time.Millisecond)
}

func TestCheckCPUNodeDoesNothingButChain(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.knownNodes("cn000")
		r := w.runCheck()
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("CPU node: code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
		}
		time.Sleep(100 * time.Millisecond)
		if got := w.stateFile("installer"); got != "" {
			t.Fatalf("a node without a GPU got an install: %q", got)
		}
		if w.exists(w.p.InstallDir) {
			t.Fatal("a node without a GPU got an install directory")
		}
	})
}

func TestCheckGPUVisibleOnlyThroughNvidiaSMIIsAGPUNode(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.flag("gpu", true) // no /dev/nvidia0, but the driver lists a card
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.runCheck()
		w.awaitInstall()
		if n := len(w.installs()); n != 1 {
			t.Fatalf("installs = %d, want 1", n)
		}
	})
}

func TestCheckPropagatesChainedStatus(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *world)
	}{
		{"cpu node", func(w *world) {}},
		{"agent active", func(w *world) { w.gpuNode(); w.flag("agent-active", true) }},
		{"install path", func(w *world) { w.gpuNode(); w.knownNodes("cn000"); w.env["FAKE_INSTALL_ACTIVATES"] = "1" }},
	}
	for _, c := range cases {
		for _, code := range []int{0, 1, 7} {
			t.Run(c.name+"/"+strconv.Itoa(code), func(t *testing.T) {
				eachShell(t, func(t *testing.T, w *world) {
					c.setup(w)
					prog := w.program("nhc", code)
					w.chain(prog)
					r := w.runCheck("--some-arg", "x")
					if r.code != code {
						t.Fatalf("exit = %d, want %d (stderr %q)", r.code, code, r.stderr)
					}
					if got := w.read(prog + ".args"); got != "--some-arg x\n" {
						t.Fatalf("chained program args = %q", got)
					}
					if c.name == "install path" {
						w.awaitInstall()
					}
				})
			})
		}
	}
}

func TestCheckChainedMissingOrNotExecutableIsZero(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("no chained file: exit %d", r.code)
		}
		w.chain(w.path("progs/gone"))
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("missing program: exit %d", r.code)
		}
		plain := w.path("progs/plain")
		w.write(plain, "#!/bin/sh\nexit 9\n") // mode 0644
		w.chain(plain)
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("not executable program: exit %d", r.code)
		}
		w.chain("") // empty first line
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("empty chained file: exit %d", r.code)
		}
	})
}

func TestCheckNeverChainsItself(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.chain(filepath.Join(w.p.ScriptDir, "check.sh"))
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("self chain: exit %d", r.code)
		}
	})
}

func TestCheckAgentActiveDoesNotInstall(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.flag("agent-active", true)
		w.runCheck()
		time.Sleep(150 * time.Millisecond)
		if got := w.stateFile("installer"); got != "" {
			t.Fatalf("installed although the agent runs: %q", got)
		}
		if w.exists(w.p.InstallDir) {
			t.Fatal("touched the install directory although the agent runs")
		}
	})
}

func TestCheckInstallPassesExactArgvAndEnvironment(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		w.awaitInstall()
		in := w.installs()
		if len(in) != 1 {
			t.Fatalf("installs = %d", len(in))
		}
		want := "argc=6\narg=--server\narg=" + testServer + "\narg=--install-dir\narg=" + w.p.InstallDir +
			"\narg=--supervisor\narg=systemd\ntoken=" + w.p.Token + "\nnode=cn000\n"
		if in[0] != want {
			t.Fatalf("installer saw:\n%s\nwant:\n%s", in[0], want)
		}
		curls := strings.Split(strings.TrimSpace(w.stateFile("curl")), "\n")
		if len(curls) != 2 || curls[0] != "curl -fsS --max-time 15 -K -" ||
			!strings.HasPrefix(curls[1], "curl -fsSL --max-time 120 "+testServer+"/install.sh -o "+w.path("run")+"/gwks-install.") {
			t.Fatalf("curl calls = %q", curls)
		}
		if left, _ := filepath.Glob(w.path("run/gwks-install.*")); len(left) != 0 {
			t.Fatalf("the downloaded installer was left behind: %v", left)
		}
		if got := w.stateFile("token-status-requests"); got != "url = \""+testServer+"/slurm/token-status?token="+w.p.Token+"\"\n" {
			t.Fatalf("token-status request = %q", got)
		}
		// The token is in the environment only: nothing that lists arguments may show it.
		for _, f := range []string{"curl", "installer"} {
			for _, line := range strings.Split(w.stateFile(f), "\n") {
				if (strings.HasPrefix(line, "curl ") || strings.HasPrefix(line, "arg=")) && strings.Contains(line, w.p.Token) {
					t.Fatalf("token on an argument list: %q", line)
				}
			}
		}
		log := w.read(filepath.Join(w.p.InstallDir, "slurm-check.log"))
		if !strings.Contains(log, "installing the agent for Slurm node cn000") || strings.Contains(log, w.p.Token) {
			t.Fatalf("log = %q", log)
		}
	})
}

func TestCheckRunAsIsPassedOnlyForNonRoot(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.p.RunAs = "gpuworks"
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.runCheck()
		w.awaitInstall()
		in := w.installs()
		if len(in) != 1 || !strings.Contains(in[0], "argc=8\n") || !strings.HasSuffix(strings.Split(in[0], "token=")[0], "arg=systemd\narg=--run-as\narg=gpuworks\n") {
			t.Fatalf("non-root install: %v", in)
		}
	})
}

func TestCheckConcurrentRunsInstallOnceAndReturnImmediately(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_SLEEP"] = "3"
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		script := w.installCheck()
		ch := make(chan result, 2)
		for i := 0; i < 2; i++ {
			go func() { ch <- w.exec("", script) }()
		}
		for i := 0; i < 2; i++ {
			r := <-ch
			if r.code != 0 || r.elapsed > 1500*time.Millisecond {
				t.Fatalf("check run: code=%d elapsed=%v (the installer sleeps 3s: it must run in the background)", r.code, r.elapsed)
			}
		}
		w.awaitInstall()
		if n := len(w.installs()); n != 1 {
			t.Fatalf("installer ran %d times, want exactly 1", n)
		}
		// With the agent up, further runs install nothing.
		w.runCheck()
		time.Sleep(200 * time.Millisecond)
		if n := len(w.installs()); n != 1 {
			t.Fatalf("installer ran %d times after the agent came up", n)
		}
	})
}

func TestCheckRetriesOnTheNextRunWhenTheInstallDidNotStartTheAgent(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.runCheck()
		w.awaitInstall()
		_ = os.Remove(filepath.Join(w.state, "installer-done"))
		w.runCheck()
		w.awaitInstall()
		if n := len(w.installs()); n != 2 {
			t.Fatalf("installs = %d, want 2 (one per run while the agent is not active)", n)
		}
	})
}

func TestCheckWithoutSetsidFallsBackToNohup(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.remove("setsid")
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_SLEEP"] = "1"
		r := w.runCheck()
		if r.code != 0 || r.elapsed > 700*time.Millisecond {
			t.Fatalf("code=%d elapsed=%v", r.code, r.elapsed)
		}
		w.awaitInstall()
		if n := len(w.installs()); n != 1 {
			t.Fatalf("installs = %d", n)
		}
	})
}

func TestCheckUninstallMode(t *testing.T) {
	t.Run("agent unit exists", func(t *testing.T) {
		eachShell(t, func(t *testing.T, w *world) {
			w.gpuNode()
			w.flag("agent-unit", true)
			w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
			agent := filepath.Join(w.p.InstallDir, "gpuworks-agent")
			w.writeExec(agent, "#!/bin/sh\necho \"$*\" >> \""+w.state+"/agent-calls\"\necho removing\n")
			prog := w.program("nhc", 3)
			w.chain(prog)
			r := w.runCheck()
			if r.code != 3 {
				t.Fatalf("exit %d, want the chained 3", r.code)
			}
			if got := w.stateFile("agent-calls"); got != "uninstall --state "+w.p.InstallDir+" --yes\n" {
				t.Fatalf("agent call = %q", got)
			}
			if !strings.Contains(w.read(filepath.Join(w.p.InstallDir, "slurm-check.log")), "removing") {
				t.Fatal("the uninstall output is not in the log")
			}
			if w.stateFile("installer") != "" {
				t.Fatal("installed in uninstall mode")
			}
		})
	})
	t.Run("no unit", func(t *testing.T) {
		eachShell(t, func(t *testing.T, w *world) {
			w.gpuNode()
			w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
			w.writeExec(filepath.Join(w.p.InstallDir, "gpuworks-agent"), "#!/bin/sh\necho called >> \""+w.state+"/agent-calls\"\n")
			if r := w.runCheck(); r.code != 0 {
				t.Fatalf("exit %d", r.code)
			}
			time.Sleep(150 * time.Millisecond)
			if w.stateFile("agent-calls") != "" || w.stateFile("installer") != "" {
				t.Fatal("did something on a node that has no agent while uninstalling")
			}
		})
	})
	t.Run("a failing uninstall never fails the node", func(t *testing.T) {
		eachShell(t, func(t *testing.T, w *world) {
			w.gpuNode()
			w.flag("agent-unit", true)
			w.write(filepath.Join(w.p.ScriptDir, "uninstall-agents"), "x\n")
			w.writeExec(filepath.Join(w.p.InstallDir, "gpuworks-agent"), "#!/bin/sh\necho boom >&2\nexit 5\n")
			if r := w.runCheck(); r.code != 0 {
				t.Fatalf("exit %d", r.code)
			}
		})
	})
}

// slurmdProc fakes the /proc entry of a running slurmd.
func (w *world) slurmdProc(pid string, argv ...string) {
	w.t.Helper()
	w.write(w.path("proc/"+pid+"/comm"), "slurmd\n")
	w.write(w.path("proc/"+pid+"/cmdline"), strings.Join(argv, "\x00")+"\x00")
	// a neighbour that must be ignored
	w.write(w.path("proc/1/comm"), "systemd\n")
	w.write(w.path("proc/1/cmdline"), "/sbin/init\x00-N\x00wrong\x00")
}

func TestCheckNonRootWritesCardOrderFromSlurmd(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.p.RunAs = "gpuworks"
		w.knownNodes("cn000")
		w.slurmdProc("4242", "/usr/sbin/slurmd", "-D", "-Z", "--conf-server", "ctl:6817", "-N", "cn000", "-f", "/etc/x.conf", "-s")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.runCheck()
		w.awaitInstall()
		if got := w.stateFile("slurmd"); got != "slurmd -G -Z --conf-server ctl:6817 -N cn000 -f /etc/x.conf\n" {
			t.Fatalf("slurmd call = %q", got)
		}
		file := filepath.Join(w.p.InstallDir, "slurm-gres-devices")
		got := w.read(file)
		if strings.Count(got, "Gres Name=gpu") != 2 || !strings.Contains(got, "File=/dev/nvidia1") {
			t.Fatalf("gres file = %q", got)
		}
		if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o644 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
		if c := w.stateFile("chown"); !strings.Contains(c, "gpuworks "+w.p.InstallDir+"\n") {
			t.Fatalf("chown calls = %q", c)
		}
		// The file is put in place by install(1) for the agent's account, from a
		// root-owned temporary file that does not stay.
		if c := w.stateFile("install"); !strings.Contains(c, "install -m 0644 -o gpuworks "+w.path("run")+"/gwks-gres.") || !strings.HasSuffix(c, " "+file+"\n") {
			t.Fatalf("install calls = %q", c)
		}
		for _, pattern := range []string{w.path("run/gwks-gres.*"), filepath.Join(w.p.InstallDir, ".slurm-gres-devices*")} {
			if ents, _ := filepath.Glob(pattern); len(ents) != 0 {
				t.Fatalf("temporary files left: %v", ents)
			}
		}
	})
}

func TestCheckCardOrderKeepsEqualsFormsAndPlainSlurmd(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.p.RunAs = "gpuworks"
		w.knownNodes("cn000")
		w.slurmdProc("77", "slurmd", "--conf=/etc/y.conf", "--conf-server=ctl", "-Ncn000")
		w.runCheck()
		w.waitFor("the gres file", func() bool { return w.exists(filepath.Join(w.p.InstallDir, "slurm-gres-devices")) })
		if got := w.stateFile("slurmd"); got != "slurmd -G --conf=/etc/y.conf --conf-server=ctl -Ncn000\n" {
			t.Fatalf("slurmd call = %q", got)
		}
		// no slurmd process at all: plain slurmd -G
		w.flag("agent-active", false)
		_ = os.RemoveAll(w.path("proc"))
		_ = os.MkdirAll(w.path("proc"), 0o755)
		_ = os.Remove(filepath.Join(w.state, "slurmd"))
		w.runCheck()
		w.waitFor("the second slurmd call", func() bool { return w.stateFile("slurmd") != "" })
		if got := w.stateFile("slurmd"); got != "slurmd -G\n" {
			t.Fatalf("slurmd call = %q", got)
		}
	})
}

func TestCheckCardOrderNeverClobbersAGoodFile(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.p.RunAs = "gpuworks"
		w.knownNodes("cn000")
		file := filepath.Join(w.p.InstallDir, "slurm-gres-devices")
		w.write(file, "slurmd: Gres Name=gpu Type=(null) Count=1 ID=1 File=/dev/nvidia7\n")
		w.flag("slurmd-fail", true)
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.runCheck()
		w.awaitInstall()
		if got := w.read(file); !strings.Contains(got, "nvidia7") || strings.Contains(got, "error") {
			t.Fatalf("a failed slurmd run replaced the file: %q", got)
		}
	})
}

func TestCheckRootDoesNotWriteCardOrder(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.runCheck()
		w.awaitInstall()
		if w.stateFile("slurmd") != "" || w.exists(filepath.Join(w.p.InstallDir, "slurm-gres-devices")) {
			t.Fatal("a root agent does not need the card order file")
		}
	})
}

func TestCheckNodeNameResolution(t *testing.T) {
	cases := []struct {
		name  string
		host  string // fake FQDN
		known []string
		argv  []string // slurmd command line, nil for no slurmd
		down  bool     // scontrol cannot answer
		want  string
	}{
		{"slurmd -N wins", "cn000.cluster.local", []string{"alias1", "cn000"}, []string{"slurmd", "-N", "alias1"}, false, "alias1"},
		{"-N unknown falls to short host name", "cn000.cluster.local", []string{"cn000"}, []string{"slurmd", "-N", "ghost"}, false, "cn000"},
		{"no slurmd: short host name", "cn000.cluster.local", []string{"cn000", "cn000.cluster.local"}, nil, false, "cn000"},
		{"full host name when only that is a node", "cn000.cluster.local", []string{"cn000.cluster.local"}, nil, false, "cn000.cluster.local"},
		{"nothing verifies: short host name", "gpu7.lab", []string{"other"}, nil, false, "gpu7"},
		{"scontrol failing: short host name", "gpu7.lab", []string{"gpu7"}, []string{"slurmd", "-N", "alias1"}, true, "gpu7"},
		{"prefix of a node is not the node", "cn0.x", []string{"cn00"}, nil, false, "cn0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.gpuNode()
				w.env["FAKE_HOSTNAME"] = c.host
				w.knownNodes(c.known...)
				w.flag("scontrol-down", c.down)
				if c.argv != nil {
					w.slurmdProc("900", c.argv...)
				}
				w.env["FAKE_INSTALL_ACTIVATES"] = "1"
				w.runCheck()
				w.awaitInstall()
				in := w.installs()
				if len(in) != 1 || !strings.Contains(in[0], "node="+c.want+"\n") {
					t.Fatalf("want node=%s, installer saw %v", c.want, in)
				}
			})
		})
	}
}

func TestCheckHonoursTheInstallDirFallbackForTheLog(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		// An install dir that cannot be created: its parent is a file.
		blocker := w.path("blocker")
		w.write(blocker, "x")
		w.p.InstallDir = blocker + "/sub"
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.env["GWKS_TMP_DIR"] = w.path("run")
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		w.awaitInstall()
		if n := len(w.installs()); n != 1 {
			t.Fatalf("installs = %d: an unwritable install dir must not stop the install", n)
		}
		if !strings.Contains(w.read(w.path("run/gpuworks-slurm-check.log")), "installing the agent") {
			t.Fatal("the log did not fall back to the temporary directory")
		}
	})
}

// The token lives in a root-only file next to the script, never in the script
// that every user on every node can read.
func TestCheckScriptCarriesNoTokenAndInstallReadsTheFile(t *testing.T) {
	if strings.Contains(CheckScript(Params{ServerURL: testServer, Token: "tok-abc.123_X", RunAs: "root", InstallDir: "/i", ScriptDir: "/s"}), "tok-abc.123_X") {
		t.Fatal("the check script embeds the enrollment token")
	}
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.p.Token = "other-token_1" // the file, not the script, is the source
		w.env["FAKE_TOKEN"] = w.p.Token
		w.installCheck()
		w.write(filepath.Join(w.p.ScriptDir, "token"), w.p.Token+"\n")
		if r := w.exec("", filepath.Join(w.p.ScriptDir, "check.sh")); r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		w.awaitInstall()
		in := w.installs()
		if len(in) != 1 || !strings.Contains(in[0], "token=other-token_1\n") {
			t.Fatalf("installer saw %v", in)
		}
	})
}

func TestCheckCannotReadTheTokenLogsAndInstallsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, w *world)
	}{
		{"missing file", func(t *testing.T, w *world) { w.noToken = true }},
		{"empty file", func(t *testing.T, w *world) {
			w.write(filepath.Join(w.p.ScriptDir, "token"), "")
		}},
		{"unreadable file", func(t *testing.T, w *world) {
			if os.Geteuid() == 0 {
				t.Skip("root reads anything")
			}
			w.write(filepath.Join(w.p.ScriptDir, "token"), w.p.Token+"\n")
			if err := os.Chmod(filepath.Join(w.p.ScriptDir, "token"), 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.gpuNode()
				w.knownNodes("cn000")
				tc.breakIt(t, w)
				w.chain(w.program("nhc", 4))
				r := w.runCheck()
				time.Sleep(200 * time.Millisecond)
				if r.code != 4 {
					t.Fatalf("exit %d, want the chained 4: an unreadable token never drains a node", r.code)
				}
				if w.stateFile("installer") != "" || w.stateFile("curl") != "" {
					t.Fatalf("installed or called the server without a token: %q %q", w.stateFile("installer"), w.stateFile("curl"))
				}
				log := w.read(filepath.Join(w.p.InstallDir, "slurm-check.log"))
				if !strings.Contains(log, "cannot read the enrollment token "+w.p.ScriptDir+"/token") || !strings.Contains(log, "root-squashed") {
					t.Fatalf("log = %q", log)
				}
			})
		})
	}
}

// Only an active token installs: a revoked or unknown one, a server that
// does not answer, and an answer that is not the documented one all leave the
// node without an agent, and none of them fails the check.
func TestCheckInstallsOnlyWhileTheTokenIsActive(t *testing.T) {
	cases := []struct {
		name, status string
		down         bool
		wantLog      string
	}{
		{"revoked", `{"status":"revoked"}`, false, `the server said: {"status":"revoked"}`},
		{"unknown", `{"status":"unknown"}`, false, `the server said: {"status":"unknown"}`},
		{"garbage", `<html>502 Bad Gateway</html>`, false, "not installing"},
		{"server down", "", true, "the server said: nothing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.gpuNode()
				w.knownNodes("cn000")
				w.chain(w.program("nhc", 2))
				if c.down {
					w.flag("token-status-down", true)
				} else {
					w.write(filepath.Join(w.state, "token-status"), c.status+"\n")
				}
				if r := w.runCheck(); r.code != 2 {
					t.Fatalf("exit %d, want the chained 2", r.code)
				}
				w.waitFor("the token question", func() bool { return w.stateFile("token-status-requests") != "" })
				time.Sleep(300 * time.Millisecond)
				if w.stateFile("installer") != "" || strings.Contains(w.stateFile("curl"), "install.sh") {
					t.Fatalf("installed for a token that is not active: %q", w.stateFile("installer"))
				}
				log := w.read(filepath.Join(w.p.InstallDir, "slurm-check.log"))
				if !strings.Contains(log, "the enrollment token is not active") || !strings.Contains(log, c.wantLog) || strings.Contains(log, w.p.Token) {
					t.Fatalf("log = %q", log)
				}
			})
		})
	}
}

// The token is asked about on curl's standard input, not its argument list.
func TestCheckKeepsTheTokenOffEveryArgumentList(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.env["FAKE_INSTALL_ACTIVATES"] = "1"
		w.runCheck()
		w.awaitInstall()
		if strings.Contains(w.stateFile("curl"), w.p.Token) {
			t.Fatalf("token on a curl argument list: %q", w.stateFile("curl"))
		}
	})
}
