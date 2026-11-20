package slurmboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// activeHC counts the uncommented HealthCheck* settings of a slurm.conf, by
// key (slurm.conf keys are case insensitive).
func activeHC(conf string) map[string]int {
	n := map[string]int{}
	for _, line := range strings.Split(conf, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		for _, k := range []string{"healthcheckprogram", "healthcheckinterval", "healthchecknodestate"} {
			if strings.HasPrefix(l, k) {
				n[k]++
			}
		}
	}
	return n
}

var confShapes = []struct{ name, conf string }{
	{"no health check", baseConf},
	{"program only", baseConf + "HealthCheckProgram=/usr/sbin/nhc\n"},
	{"scattered settings, mixed case and comments", "# head\nHealthCheckProgram=/usr/sbin/nhc  # node health check\n" +
		"ClusterName=demo\n#HealthCheckProgram=/old/commented\nSlurmctldParameters=enable_configless\n" +
		"  healthcheckinterval = 60\nNodeName=cn[000-002] CPUs=16 Gres=gpu:1\n\tHealthCheckNodeState=IDLE,CYCLE\n" +
		"PartitionName=gpu Nodes=ALL\n"},
	{"our own program configured by hand", baseConf + "HealthCheckProgram=@SCRIPT@/check.sh\nHealthCheckInterval=30\n"},
	{"empty file", ""},
	{"only comments", "# nothing here\n\n"},
}

func TestSetupThenRemoveRestoresTheOriginalByteForByte(t *testing.T) {
	for _, shape := range confShapes {
		for _, eol := range []bool{true, false} {
			name := shape.name + "/final newline"
			if !eol {
				name = shape.name + "/no final newline"
			}
			t.Run(name, func(t *testing.T) {
				eachShell(t, func(t *testing.T, w *world) {
					orig := strings.ReplaceAll(shape.conf, "@SCRIPT@", w.p.ScriptDir)
					if !eol {
						orig = strings.TrimSuffix(orig, "\n")
					}
					w.controller("nfs", orig)
					r := w.runSetup()
					if r.code != 0 {
						t.Fatalf("setup exit %d\n%s", r.code, r.stdout)
					}
					mid := w.read(w.confPath())
					if strings.Count(mid, "# BEGIN gpuworks\n") != 1 || !strings.HasSuffix(mid, "# END gpuworks\n") {
						t.Fatalf("block not at the end:\n%s", mid)
					}
					if n := activeHC(mid); n["healthcheckprogram"] != 1 || n["healthcheckinterval"] != 1 || n["healthchecknodestate"] != 1 {
						t.Fatalf("active settings = %v in\n%s", n, mid)
					}
					if !strings.Contains(mid, "\nHealthCheckProgram="+w.p.ScriptDir+"/check.sh\n") || !strings.Contains(mid, "\nHealthCheckNodeState=ANY\n") {
						t.Fatalf("block content:\n%s", mid)
					}
					r = w.runSetup("--remove")
					if r.code != 0 {
						t.Fatalf("remove exit %d\n%s", r.code, r.stdout)
					}
					if got := w.read(w.confPath()); got != orig {
						t.Fatalf("after setup + remove:\n%q\nwant\n%q", got, orig)
					}
					if m := marker(t, r.stdout, "remove"); m["status"] != "ok" || m["path"] != w.confPath() {
						t.Fatalf("remove marker = %v", m)
					}
					if marker(t, r.stdout, "reconfigure")["status"] != "ok" || marker(t, r.stdout, "done")["status"] != "ok" {
						t.Fatalf("markers:\n%s", r.stdout)
					}
					if got := strings.Count(w.stateFile("reconfigured"), "reconfigure"); got != 2 {
						t.Fatalf("reconfigures = %d, want one per command", got)
					}
					// setup + remove + setup works again, and its remove restores again
					w.runSetup()
					w.runSetup("--remove")
					if got := w.read(w.confPath()); got != orig {
						t.Fatalf("second round trip:\n%q\nwant\n%q", got, orig)
					}
				})
			})
		}
	}
}

func TestSetupMovesAnExistingHealthCheckBehindOurs(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		conf := baseConf + "HealthCheckProgram=" + w.program("nhc", 7) + "\nHealthCheckInterval=60\nHealthCheckNodeState=IDLE\n"
		w.controller("nfs", conf)
		r := w.runSetup()
		prog := w.path("progs/nhc")
		if got := w.read(filepath.Join(w.p.ScriptDir, "chained")); got != prog+"\nIDLE\n" {
			t.Fatalf("chained = %q", got)
		}
		if m := marker(t, r.stdout, "chained"); m["program"] != prog || m["node_state"] != "IDLE" {
			t.Fatalf("chained marker = %v", m)
		}
		got := w.read(w.confPath())
		for _, line := range []string{
			"#gpuworks-original# HealthCheckProgram=" + prog,
			"#gpuworks-original# HealthCheckInterval=60",
			"#gpuworks-original# HealthCheckNodeState=IDLE",
			"HealthCheckInterval=60",
		} {
			if !strings.Contains(got, line+"\n") {
				t.Fatalf("missing %q in\n%s", line, got)
			}
		}
		if m := marker(t, r.stdout, "slurm_conf"); m["chained"] != prog || m["interval"] != "60" {
			t.Fatalf("slurm_conf = %v", m)
		}
		// the health check we wrote really runs the old one and returns its status
		w.env["FAKE_HOSTNAME"] = "cn000.cluster.local"
		code := w.exec("", filepath.Join(w.p.ScriptDir, "check.sh"), "a1")
		if code.code != 7 || w.read(prog+".args") != "a1\n" {
			t.Fatalf("check.sh exit %d, nhc args %q", code.code, w.stateFile("x"))
		}
		if n := activeHC(got); n["healthcheckprogram"] != 1 {
			t.Fatalf("active settings = %v", n)
		}
	})
}

func TestSetupDoesNotChainItselfWhenTheConfAlreadyNamesOurScript(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf+"HealthCheckProgram="+w.p.ScriptDir+"/check.sh\n")
		w.runSetup()
		if w.exists(filepath.Join(w.p.ScriptDir, "chained")) {
			t.Fatal("chained to ourselves")
		}
	})
}

func TestSetupHealthCheckInterval(t *testing.T) {
	cases := []struct {
		name  string
		conf  string
		args  []string
		want  string
		again []string // a second run, whose interval is wanted too
		want2 string
	}{
		{"default when none", baseConf, nil, "300", nil, ""},
		{"existing one is kept", baseConf + "HealthCheckInterval=120\n", nil, "120", nil, ""},
		{"existing zero is no interval", baseConf + "HealthCheckInterval=0\n", nil, "300", nil, ""},
		{"explicit wins over existing", baseConf + "HealthCheckInterval=120\n", []string{"--interval", "45"}, "45", nil, ""},
		{"explicit with equals sign", baseConf, []string{"--interval=45"}, "45", nil, ""},
		{"earlier explicit choice survives a plain re-run", baseConf, []string{"--interval", "45"}, "45", []string{}, "45"},
		{"a later explicit choice replaces it", baseConf, []string{"--interval", "45"}, "45", []string{"--interval", "90"}, "90"},
		{"existing interval beats our earlier choice", baseConf + "HealthCheckInterval=200\n", nil, "200", []string{}, "200"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller("nfs", c.conf)
				r := w.runSetup(c.args...)
				if m := marker(t, r.stdout, "slurm_conf"); m["interval"] != c.want {
					t.Fatalf("interval = %s, want %s", m["interval"], c.want)
				}
				if !strings.Contains(w.read(w.confPath()), "\nHealthCheckInterval="+c.want+"\n# ") &&
					!strings.Contains(w.read(w.confPath()), "\nHealthCheckInterval="+c.want+"\nHealthCheckNodeState=ANY\n") {
					t.Fatalf("conf:\n%s", w.read(w.confPath()))
				}
				if c.again != nil || c.want2 != "" {
					r = w.runSetup(c.again...)
					if m := marker(t, r.stdout, "slurm_conf"); m["interval"] != c.want2 {
						t.Fatalf("second run interval = %s, want %s", m["interval"], c.want2)
					}
				}
			})
		})
	}
}

func TestSetupSecondRunWithANewIntervalBacksUpAgain(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup()
		w.runSetup("--interval", "90")
		bs := backups(t, w)
		if len(bs) != 2 {
			t.Fatalf("backups = %v: a changed file is backed up, even within the same second", bs)
		}
		if strings.Contains(w.read(bs[0]), "# BEGIN gpuworks") == strings.Contains(w.read(bs[1]), "# BEGIN gpuworks") {
			t.Fatal("the two backups should be the original and the first block")
		}
		if got := w.read(bs[0]) + w.read(bs[1]); !strings.Contains(got, baseConf) {
			t.Fatal("the original is lost")
		}
	})
}

func TestRemoveWithoutABlockIsANoop(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		r := w.runSetup("--remove")
		if r.code != 0 || !strings.Contains(r.stdout, "Nothing to remove") || marker(t, r.stdout, "remove")["status"] != "noop" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if w.read(w.confPath()) != baseConf || len(backups(t, w)) != 0 || w.stateFile("reconfigured") != "" {
			t.Fatal("a no-op remove changed something")
		}
	})
}

func TestRemoveDeletesTheUninstallFlagAndPointsToTheTwoSteps(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup()
		flag := filepath.Join(w.p.ScriptDir, "uninstall-agents")
		w.write(flag, "x\n")
		r := w.runSetup("--remove")
		if w.exists(flag) {
			t.Fatal("the uninstall flag stayed")
		}
		if !w.exists(filepath.Join(w.p.ScriptDir, "check.sh")) {
			t.Fatal("remove deleted check.sh: it leaves the script directory alone")
		}
		for _, s := range []string{"Agents already installed on the nodes keep running", "--uninstall-agents"} {
			if !strings.Contains(r.stdout, s) {
				t.Fatalf("missing %q in\n%s", s, r.stdout)
			}
		}
		if bs := backups(t, w); len(bs) != 2 {
			t.Fatalf("backups = %v, want one per edit", bs)
		}
	})
}

func TestRemoveReconfigureFailure(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup()
		w.flag("reconfigure-fail", true)
		r := w.runSetup("--remove")
		if r.code != 1 || marker(t, r.stdout, "reconfigure")["status"] != "failed" || marker(t, r.stdout, "done")["status"] != "failed" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if w.read(w.confPath()) != baseConf {
			t.Fatal("the conf was restored before the failure and stays restored")
		}
	})
}

func TestUninstallAgentsNeedsSetupFirst(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		r := w.runSetup("--uninstall-agents")
		if r.code != 1 || !strings.Contains(r.stdout, "run the setup command first") || marker(t, r.stdout, "uninstall_agents")["status"] != "failed" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if w.exists(filepath.Join(w.p.ScriptDir, "uninstall-agents")) || w.read(w.confPath()) != baseConf {
			t.Fatal("changed something without a health check installed")
		}
	})
}

func TestUninstallAgentsArmsTheFlagAndRearmedBySetup(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup("--interval", "120")
		before := w.read(w.confPath())
		nbak := len(backups(t, w))
		nrec := strings.Count(w.stateFile("reconfigured"), "reconfigure")
		r := w.runSetup("--uninstall-agents")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		flag := filepath.Join(w.p.ScriptDir, "uninstall-agents")
		if !w.exists(flag) {
			t.Fatal("no uninstall flag")
		}
		if !strings.Contains(r.stdout, "Wait one HealthCheckInterval (120 s), then run the --remove command.") {
			t.Fatalf("two-step text missing:\n%s", r.stdout)
		}
		if m := marker(t, r.stdout, "uninstall_agents"); m["status"] != "ok" || m["interval"] != "120" {
			t.Fatalf("marker = %v", m)
		}
		if w.read(w.confPath()) != before || len(backups(t, w)) != nbak || strings.Count(w.stateFile("reconfigured"), "reconfigure") != nrec {
			t.Fatal("--uninstall-agents must not touch slurm.conf or reconfigure")
		}
		// the flag really switches the generated check into uninstall mode
		w.gpuNode()
		w.flag("agent-unit", true)
		w.writeExec(filepath.Join(w.p.InstallDir, "gpuworks-agent"), "#!/bin/sh\necho \"$*\" >> \""+w.state+"/agent-calls\"\n")
		w.exec("", filepath.Join(w.p.ScriptDir, "check.sh"))
		if w.stateFile("agent-calls") != "uninstall --state "+w.p.InstallDir+" --yes\n" {
			t.Fatalf("agent calls = %q", w.stateFile("agent-calls"))
		}
		// a normal setup run re-arms install mode
		w.runSetup()
		if w.exists(flag) {
			t.Fatal("setup left the stale uninstall flag")
		}
	})
}

func TestSetupSeesNoBlockInsideAHandEditedFileAsFresh(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup()
		// an administrator deleted our block by hand but a stale original comment stayed
		conf := strings.Split(w.read(w.confPath()), "# BEGIN gpuworks")[0] + "#gpuworks-original# HealthCheckProgram=/usr/sbin/nhc\n"
		if err := os.WriteFile(w.confPath(), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
		w.runSetup()
		if got := w.read(filepath.Join(w.p.ScriptDir, "chained")); got != "/usr/sbin/nhc\nANY\n" {
			t.Fatalf("chained = %q", got)
		}
	})
}
