package slurmboot

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// snapshot is every file and directory under the fake machine except the
// fakes' own call logs (state), with mode and a digest of the content: two
// snapshots are equal exactly when nothing under the machine changed.
func (w *world) snapshot() map[string]string {
	w.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(w.root, path)
		if rel == "state" || strings.HasPrefix(rel, "state"+string(filepath.Separator)) || rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		desc := info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			desc += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		out[rel] = desc
		return nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func diffSnapshots(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		if before[k] != v {
			out = append(out, "changed or new: "+k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, "gone: "+k)
		}
	}
	sort.Strings(out)
	return out
}

// checkShapes cover what the check must report differently: a cluster with no
// health check, one with its own (chained, commented in place), one whose
// include hides settings, and one already set up.
func checkShapes(w *world) []struct{ name, conf string } {
	nhc := w.program("nhc", 0)
	return []struct{ name, conf string }{
		{"no health check", baseConf},
		{"own health check", baseConf + "HealthCheckProgram=" + nhc + "\nHealthCheckInterval=60\nHealthCheckNodeState=IDLE,CYCLE\n"},
		{"scattered settings without a final newline", strings.TrimSuffix("# head\nHealthCheckProgram="+nhc+"  # nhc\n"+baseConf+"  healthcheckinterval = 45\n", "\n")},
		{"with an include", baseConf + "Include /etc/slurm/extra.conf\n"},
	}
}

// --check writes nothing: no file under the controller, no copy to a node, no
// reconfigure, whatever the cluster looks like and whether or not it is
// shared storage. The same run without --check then does change it.
func TestCheckChangesNothing(t *testing.T) {
	for _, fstype := range []string{"nfs4", "ext4"} {
		eachShell(t, func(t *testing.T, w *world) {
			w.onlyRemoteTools("ssh", "scp")
			for _, shape := range checkShapes(w) {
				w.controller(fstype, shape.conf)
				w.write(filepath.Join(w.p.ScriptDir, "old-file"), "an existing file\n")
				before := w.snapshot()
				r := w.runSetup("--check")
				if r.code != 0 {
					t.Fatalf("%s/%s: --check exit %d\n%s%s", fstype, shape.name, r.code, r.stdout, r.stderr)
				}
				if d := diffSnapshots(before, w.snapshot()); len(d) != 0 {
					t.Fatalf("%s/%s: --check changed the machine: %v\n%s", fstype, shape.name, d, r.stdout)
				}
				if got := w.stateFile("reconfigured"); got != "" {
					t.Fatalf("%s/%s: --check reconfigured: %q", fstype, shape.name, got)
				}
				for _, tool := range []string{"ssh", "scp", "pdsh", "pdcp", "clush"} {
					if w.calls(tool) != 0 {
						t.Fatalf("%s/%s: --check called %s:\n%s", fstype, shape.name, tool, w.stateFile("calls"))
					}
				}
				if m := marker(t, r.stdout, "done"); m["status"] != "ok" && m["status"] != "partial" || m["mode"] != "check" {
					t.Fatalf("done = %v", m)
				}
				if !strings.Contains(r.stdout, "CHECK ONLY") {
					t.Fatalf("the output does not say it only checks:\n%s", r.stdout)
				}
			}
		})
	}
}

// The plan --check reports is what the run then does: the same chained
// program and states, interval, filesystem, nodes copied to, files, settings
// commented and slurm.conf left, byte for byte.
func TestCheckReportsThePlanTheInstallThenExecutes(t *testing.T) {
	for _, fstype := range []string{"nfs4", "ext4"} {
		eachShell(t, func(t *testing.T, w *world) {
			w.onlyRemoteTools("ssh", "scp")
			for _, shape := range checkShapes(w) {
				w.controller(fstype, shape.conf)
				w.write(w.path("etc/slurm/extra.conf"), "# nothing\n")
				planned := w.runSetup("--check", "--interval", "120")
				if planned.code != 0 {
					t.Fatalf("%s/%s: check exit %d\n%s", fstype, shape.name, planned.code, planned.stdout)
				}
				before := w.read(w.confPath())
				ran := w.runSetup("--interval", "120")
				if ran.code != 0 {
					t.Fatalf("%s/%s: install exit %d\n%s", fstype, shape.name, ran.code, ran.stdout)
				}
				name := fstype + "/" + shape.name

				for _, step := range []string{"precheck", "shared"} {
					p, a := marker(t, planned.stdout, step), marker(t, ran.stdout, step)
					for _, k := range []string{"status", "cluster", "fstype"} {
						if p[k] != a[k] {
							t.Errorf("%s: %s %s: planned %q, ran %q", name, step, k, p[k], a[k])
						}
					}
				}
				pc, ac := marker(t, planned.stdout, "chained"), marker(t, ran.stdout, "chained")
				if pc["status"] != "plan" || pc["program"] != ac["program"] || pc["node_state"] != ac["node_state"] {
					t.Errorf("%s: chained planned %v, ran %v", name, pc, ac)
				}
				pk, ak := marker(t, planned.stdout, "check_script"), marker(t, ran.stdout, "check_script")
				if pk["path"] != ak["path"] || marker(t, planned.stdout, "token_file")["path"] != marker(t, ran.stdout, "token_file")["path"] {
					t.Errorf("%s: script paths planned %v, ran %v", name, pk, ak)
				}
				ps, as := marker(t, planned.stdout, "slurm_conf"), marker(t, ran.stdout, "slurm_conf")
				if ps["status"] != "plan" || ps["path"] != as["path"] || ps["chained"] != as["chained"] || ps["interval"] != as["interval"] || ps["interval"] != "120" {
					t.Errorf("%s: slurm_conf planned %v, ran %v", name, ps, as)
				}
				if ps["changed"] != "yes" {
					t.Errorf("%s: a conf with no block is not changed by the install? %v", name, ps)
				}
				// The block the install writes is the one named: its settings, and the
				// original settings it turned into comments are the ones listed.
				after := w.read(w.confPath())
				for _, want := range []string{"HealthCheckProgram=" + w.p.ScriptDir + "/check.sh", "HealthCheckInterval=" + ps["interval"], "HealthCheckNodeState=" + ps["node_state"]} {
					if !strings.Contains(after, "\n"+want+"\n") {
						t.Errorf("%s: the installed slurm.conf lacks planned line %q:\n%s", name, want, after)
					}
				}
				var wantComments []string
				for i, line := range strings.Split(before, "\n") {
					l := strings.ToLower(strings.TrimSpace(line))
					for _, k := range []string{"healthcheckprogram", "healthcheckinterval", "healthchecknodestate"} {
						if strings.HasPrefix(l, k) {
							wantComments = append(wantComments, fmt.Sprint(i+1))
							if !strings.Contains(after, ORIGINAL+line+"\n") {
								t.Errorf("%s: line %d %q was not commented in place", name, i+1, line)
							}
						}
					}
				}
				var gotComments []string
				for _, m := range markers(t, planned.stdout, "conf_comment") {
					gotComments = append(gotComments, m["line"])
				}
				if strings.Join(gotComments, ",") != strings.Join(wantComments, ",") {
					t.Errorf("%s: planned comments on lines %v, the install commented %v", name, gotComments, wantComments)
				}
				// Distribution: the nodes named are the nodes copied to.
				pd := marker(t, planned.stdout, "distribute_plan")
				if as := marker(t, ran.stdout, "shared")["status"]; as == "yes" {
					if pd["skipped"] != "shared" || pd["nodes"] != "none" {
						t.Errorf("%s: shared plan = %v", name, pd)
					}
				} else {
					var copied []string
					for _, m := range markers(t, ran.stdout, "distribute") {
						copied = append(copied, m["node"])
					}
					if pd["nodes"] != strings.Join(copied, ",") || pd["skipped"] != "" {
						t.Errorf("%s: planned nodes %q, copied to %v", name, pd["nodes"], copied)
					}
					wantFiles := "check.sh,token"
					if ac["program"] != "none" {
						wantFiles += ",chained"
					}
					if pd["files"] != wantFiles {
						t.Errorf("%s: planned files %q, want %q", name, pd["files"], wantFiles)
					}
				}
				if (marker(t, planned.stdout, "configless")["value"]) != marker(t, ran.stdout, "configless")["value"] {
					t.Errorf("%s: configless differs", name)
				}
				// The same plan again, now that the install is done, finds nothing to change.
				again := w.runSetup("--check", "--interval", "120")
				if m := marker(t, again.stdout, "slurm_conf"); m["changed"] != "no" {
					t.Errorf("%s: after the install --check still plans a change: %v", name, m)
				}
				// Put the cluster back for the next shape.
				w.runSetup("--remove")
				_ = os.RemoveAll(w.p.ScriptDir)
			}
		})
	}
}

// ORIGINAL is how the install marks a setting it commented in place.
const ORIGINAL = "#gpuworks-original# "

// The warnings of the install are the check's: an include it cannot see into,
// a program slurm.conf does not name, a cluster that is not configless. Each
// makes the check partial, as it makes the install partial.
func TestCheckWarnsWhereTheInstallWould(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf+"Include /etc/slurm/extra.conf\n")
		w.env["FAKE_CTLD_PARAMS"] = "none"
		r := w.runSetup("--check")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if m := marker(t, r.stdout, "conf_include"); m["status"] != "warn" || m["files"] != "/etc/slurm/extra.conf" {
			t.Fatalf("conf_include = %v", m)
		}
		if m := marker(t, r.stdout, "configless"); m["value"] != "no" {
			t.Fatalf("configless = %v", m)
		}
		if m := marker(t, r.stdout, "done"); m["status"] != "partial" || m["mode"] != "check" {
			t.Fatalf("done = %v", m)
		}
	})
}

// A program slurmctld runs that slurm.conf does not name is reported, as
// the install reports it.
func TestCheckSaysWhenSlurmctldRunsAProgramTheFileDoesNotName(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.env["FAKE_ORIG_PROGRAM"] = w.program("nhc", 0)
		w.env["FAKE_ORIG_STATE"] = "IDLE"
		r := w.runSetup("--check")
		m := marker(t, r.stdout, "conf_source")
		if m["status"] != "warn" || m["reason"] != "program_not_in_conf" || m["program"] != w.path("progs/nhc") {
			t.Fatalf("conf_source = %v\n%s", m, r.stdout)
		}
		if c := marker(t, r.stdout, "chained"); c["program"] != w.path("progs/nhc") || c["node_state"] != "IDLE" {
			t.Fatalf("chained = %v", c)
		}
	})
}

// The precheck refuses a check as it refuses an install, and --check does
// not combine with the removal modes.
func TestCheckRefusesWhatTheInstallRefuses(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.env["FAKE_UID"] = "1000"
		r := w.runSetup("--check")
		if r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "not_root" || marker(t, r.stdout, "done")["status"] != "failed" {
			t.Fatalf("not root: exit %d\n%s", r.code, r.stdout)
		}
		w.env["FAKE_UID"] = "0"
		w.env["FAKE_HOSTNAME"] = "elsewhere.cluster.local"
		w.env["FAKE_CTLD_HOST"] = "ctl"
		if r := w.runSetup("--check"); r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "not_slurmctld_host" {
			t.Fatalf("not the controller: exit %d\n%s", r.code, r.stdout)
		}
		w.env["FAKE_HOSTNAME"] = "ctl.cluster.local"
		for _, args := range [][]string{{"--check", "--remove"}, {"--uninstall-agents", "--check"}} {
			if r := w.runSetup(args...); r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "bad_arguments" {
				t.Fatalf("%v: exit %d\n%s", args, r.code, r.stdout)
			}
		}
	})
}
