package slurmboot

import (
	"path/filepath"
	"strings"
	"testing"
)

// The program the cluster runs is what slurmctld says, not what slurm.conf
// holds: here it lives in a file the conf includes.
func TestSetupChainsTheProgramSlurmctldRunsEvenWhenTheConfDoesNotName(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		prog := w.program("nhc", 5)
		w.controller("nfs", baseConf+"Include "+w.path("etc/slurm/health.conf")+"\n")
		w.env["FAKE_ORIG_PROGRAM"] = prog
		w.env["FAKE_ORIG_STATE"] = "IDLE,CYCLE"
		w.env["FAKE_ORIG_INTERVAL"] = "120"
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if got := w.read(filepath.Join(w.p.ScriptDir, "chained")); got != prog+"\nIDLE,CYCLE\n" {
			t.Fatalf("chained = %q", got)
		}
		conf := w.read(w.confPath())
		if !strings.Contains(conf, "HealthCheckInterval=120\n") || !strings.Contains(conf, "HealthCheckNodeState=ANY,CYCLE\n") {
			t.Fatalf("the block keeps the cluster's interval and its cycling, and runs in any state:\n%s", conf)
		}
		if m := marker(t, r.stdout, "conf_source"); m["status"] != "warn" || m["reason"] != "program_not_in_conf" || m["program"] != prog {
			t.Fatalf("conf_source = %v", m)
		}
		if m := marker(t, r.stdout, "conf_include"); m["status"] != "warn" || m["files"] != w.path("etc/slurm/health.conf") {
			t.Fatalf("conf_include = %v", m)
		}
		if !strings.Contains(r.stdout, "WARNING:") || !strings.Contains(r.stdout, "includes") {
			t.Fatalf("no loud warning:\n%s", r.stdout)
		}
		// setup still went through; the run asks for a look because of the program
		// that is set where we cannot edit it
		if m := marker(t, r.stdout, "done"); m["status"] != "partial" {
			t.Fatalf("done = %v", m)
		}
	})
}

// An Include alone warns and changes nothing else: setup goes through and the
// result is verified.
func TestSetupWarnsAboutIncludesAndStillWorks(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf+"include /etc/slurm/nodes.conf\nInclude /etc/slurm/other.conf # more\n")
		r := w.runSetup()
		if m := marker(t, r.stdout, "conf_include"); m["files"] != "/etc/slurm/nodes.conf,/etc/slurm/other.conf" {
			t.Fatalf("conf_include = %v\n%s", m, r.stdout)
		}
		if m := marker(t, r.stdout, "verify"); m["status"] != "ok" {
			t.Fatalf("verify = %v", m)
		}
		if m := marker(t, r.stdout, "done"); m["status"] != "ok" {
			t.Fatalf("done = %v\n%s", m, r.stdout)
		}
	})
}

// After the reconfigure slurmctld is asked again: a controller that still
// runs something else makes the run partial and says what it runs.
func TestSetupVerifiesTheHealthCheckTookEffect(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.flag("config-frozen", true)
		w.env["FAKE_ORIG_PROGRAM"] = "/usr/sbin/other-check"
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d: a health check slurmctld has not taken is partial, not failed", r.code)
		}
		m := marker(t, r.stdout, "verify")
		if m["status"] != "partial" || m["reason"] != "program_not_applied" || m["program"] != "/usr/sbin/other-check" {
			t.Fatalf("verify = %v", m)
		}
		if d := marker(t, r.stdout, "done"); d["status"] != "partial" {
			t.Fatalf("done = %v", d)
		}
	})
	t.Run("remove", func(t *testing.T) {
		eachShell(t, func(t *testing.T, w *world) {
			w.controller("nfs", baseConf)
			w.runSetup()
			w.flag("config-frozen", true)
			w.env["FAKE_ORIG_PROGRAM"] = w.p.ScriptDir + "/check.sh"
			r := w.runSetup("--remove")
			if m := marker(t, r.stdout, "verify"); m["status"] != "partial" || m["reason"] != "check_still_applied" {
				t.Fatalf("verify = %v\n%s", m, r.stdout)
			}
		})
	})
}

// A run on a machine that is not the controller edits nothing: the file
// `scontrol show config` names there is a configless slurmd's own copy, or
// the machine is no slurmctld host at all.
func TestSetupRefusesWhereSlurmctldIsNotRunning(t *testing.T) {
	cases := []struct {
		name, reason string
		arrange      func(w *world)
	}{
		{"configless slurmd cache", "not_controller_conf", func(w *world) {
			w.controller("nfs", baseConf)
			cache := w.path("run/slurm/conf/slurm.conf")
			w.write(cache, baseConf)
			w.env["FAKE_SLURM_CONF"] = cache
		}},
		{"conf-cache under the spool directory", "not_controller_conf", func(w *world) {
			w.controller("nfs", baseConf)
			cache := w.path("var/spool/slurmd/conf-cache/slurm.conf")
			w.write(cache, baseConf)
			w.env["FAKE_SLURM_CONF"] = cache
		}},
		{"another host", "not_slurmctld_host", func(w *world) {
			w.controller("nfs", baseConf)
			w.env["FAKE_HOSTNAME"] = "cn001.cluster.local"
			w.env["FAKE_CTLD_HOST"] = "ctl"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				tc.arrange(w)
				before := w.read(w.env["FAKE_SLURM_CONF"])
				r := w.runSetup()
				if r.code != 1 {
					t.Fatalf("exit %d\n%s", r.code, r.stdout)
				}
				if m := marker(t, r.stdout, "precheck"); m["status"] != "failed" || m["reason"] != tc.reason {
					t.Fatalf("precheck = %v", m)
				}
				if w.read(w.env["FAKE_SLURM_CONF"]) != before || len(backups(t, w)) != 0 || w.exists(filepath.Join(w.p.ScriptDir, "check.sh")) {
					t.Fatal("a refused run changed something")
				}
			})
		})
	}
}

// A controller is recognised by the slurmctld process, by its name in
// SlurmctldHost (short or full) and by its address; a backup controller too.
func TestSetupRecognisesTheControllerHost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(w *world)
	}{
		{"slurmctld process under another name", func(w *world) {
			w.env["FAKE_CTLD_HOST"] = "alias"
			w.write(w.path("proc/777/comm"), "slurmctld\n")
		}},
		{"full host name in the configuration", func(w *world) { w.env["FAKE_CTLD_HOST"] = "ctl.cluster.local" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, w *world) {
				w.controller("nfs", baseConf)
				tc.arrange(w)
				if r := w.runSetup(); r.code != 0 {
					t.Fatalf("exit %d\n%s", r.code, r.stdout)
				}
			})
		})
	}
}

// The check script is written with the interval the cluster runs at: it
// spreads its work over half of it.
func TestSetupBakesTheIntervalIntoTheCheckScript(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf+"HealthCheckProgram="+w.program("nhc", 0)+"\nHealthCheckInterval=90\n")
		w.runSetup()
		if body := w.read(filepath.Join(w.p.ScriptDir, "check.sh")); !strings.Contains(body, "\nINTERVAL=90\n") || strings.Contains(body, "\nINTERVAL=300\n") {
			t.Fatalf("the check script does not carry the interval:\n%s", body[:400])
		}
	})
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup("--interval", "45")
		if body := w.read(filepath.Join(w.p.ScriptDir, "check.sh")); !strings.Contains(body, "\nINTERVAL=45\n") {
			t.Fatal("an explicit interval is not in the check script")
		}
	})
}

// A second run, once our block is in: slurmctld now reports our check, and
// slurm.conf has no original line (the original lives in the included
// file), so the program the first run chained is kept, not dropped
// (2026-11-18 review: a re-run stopped the cluster's NHC).
func TestSetupRerunKeepsAProgramChainedFromAnIncludedFile(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		prog := w.program("nhc", 5)
		w.controller("nfs", baseConf+"Include "+w.path("etc/slurm/health.conf")+"\n")
		w.env["FAKE_ORIG_PROGRAM"] = prog
		w.env["FAKE_ORIG_STATE"] = "IDLE,CYCLE"
		w.env["FAKE_ORIG_INTERVAL"] = "120"
		if r := w.runSetup(); r.code != 0 {
			t.Fatalf("first run: exit %d\n%s", r.code, r.stdout)
		}
		w.env["FAKE_ORIG_PROGRAM"] = filepath.Join(w.p.ScriptDir, "check.sh")
		w.env["FAKE_ORIG_STATE"] = "ANY,CYCLE"
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("second run: exit %d\n%s", r.code, r.stdout)
		}
		if got := w.read(filepath.Join(w.p.ScriptDir, "chained")); got != prog+"\nIDLE,CYCLE\n" {
			t.Fatalf("after a re-run chained = %q; want the included program kept", got)
		}
		if m := marker(t, r.stdout, "chained"); m["program"] != prog {
			t.Fatalf("chained marker = %v", m)
		}
	})
}
