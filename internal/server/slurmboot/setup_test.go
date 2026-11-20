package slurmboot

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const baseConf = `# slurm.conf for demo
ClusterName=demo
SlurmctldHost=ctl
SlurmctldParameters=enable_configless
NodeName=cn[000-002] CPUs=16 Gres=gpu:1 State=UNKNOWN
PartitionName=gpu Nodes=ALL Default=YES MaxTime=INFINITE State=UP
`

func (w *world) confPath() string { return w.path("etc/slurm/slurm.conf") }

// controller makes this machine the Slurm controller of a cluster whose
// script directory lives on the given filesystem type.
func (w *world) controller(fstype, conf string) {
	w.t.Helper()
	w.write(w.confPath(), conf)
	w.env["FAKE_HOSTNAME"] = "ctl.cluster.local"
	w.env["FAKE_FINDMNT"] = fstype
	// sinfo -N lists a node once per partition it is in, and the controller too
	w.write(filepath.Join(w.state, "sinfo"), "cn000\ncn000\ncn001\ncn001\ncn002\nctl\n")
}

// marker returns the key=value pairs of the last GWKS_SLURM line of a step.
func marker(t *testing.T, out, step string) map[string]string {
	t.Helper()
	all := markers(t, out, step)
	if len(all) == 0 {
		t.Fatalf("no GWKS_SLURM step=%s line in:\n%s", step, out)
	}
	return all[len(all)-1]
}

func markers(t *testing.T, out, step string) []map[string]string {
	t.Helper()
	var all []map[string]string
	for _, line := range strings.Split(out, "\n") {
		if m := parseMarker(t, line); m != nil && m["step"] == step {
			all = append(all, m)
		}
	}
	return all
}

// parseMarker parses a GWKS_SLURM line, insisting on the documented shape:
// space separated key=value pairs, nothing else.
func parseMarker(t *testing.T, line string) map[string]string {
	t.Helper()
	if !strings.HasPrefix(line, "GWKS_SLURM") {
		return nil
	}
	m := map[string]string{}
	for _, f := range strings.Fields(strings.TrimPrefix(line, "GWKS_SLURM")) {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" || v == "" {
			t.Fatalf("malformed marker field %q in %q", f, line)
		}
		m[k] = v
	}
	if line != "GWKS_SLURM "+strings.Join(strings.Fields(strings.TrimPrefix(line, "GWKS_SLURM")), " ") {
		t.Fatalf("marker with odd spacing: %q", line)
	}
	return m
}

func backups(t *testing.T, w *world) []string {
	t.Helper()
	m, err := filepath.Glob(w.confPath() + ".gpuworks-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(m)
	return m
}

func TestSetupFreshCluster(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		check := filepath.Join(w.p.ScriptDir, "check.sh")
		if got := w.read(check); got != CheckScript(w.p) {
			t.Fatal("check.sh is not the embedded CheckScript byte for byte")
		}
		if fi, _ := os.Stat(check); fi.Mode().Perm() != 0o755 {
			t.Fatalf("check.sh mode = %v", fi.Mode().Perm())
		}
		if left, _ := filepath.Glob(filepath.Join(w.p.ScriptDir, ".*")); len(left) != 0 {
			t.Fatalf("temporary files left: %v", left)
		}
		want := baseConf + "# BEGIN gpuworks\n" +
			"# Managed by gpuworks (the setup script). Remove it with the --remove command from the console.\n" +
			"# Any HealthCheck* setting that was here before is kept as a \"#gpuworks-original#\" comment; its program is run by check.sh,\n" +
			"# in the node states its HealthCheckNodeState named. This one is ANY (plus CYCLE when it had it): the install runs in every state.\n" +
			"# The interval keeps the cluster existing one, else 300 seconds: an agent that stops is restored by the next run.\n" +
			"HealthCheckProgram=" + check + "\nHealthCheckInterval=300\nHealthCheckNodeState=ANY\n# END gpuworks\n"
		if got := w.read(w.confPath()); got != want {
			t.Fatalf("slurm.conf:\n%s\nwant:\n%s", got, want)
		}
		bs := backups(t, w)
		if len(bs) != 1 || w.read(bs[0]) != baseConf {
			t.Fatalf("backups = %v", bs)
		}
		if got := w.stateFile("reconfigured"); got != "reconfigure\n" {
			t.Fatalf("reconfigure calls = %q", got)
		}
		if w.exists(filepath.Join(w.p.ScriptDir, "chained")) {
			t.Fatal("a chained file without an original health check")
		}
		o := r.stdout
		if m := marker(t, o, "precheck"); m["status"] != "ok" || m["cluster"] != "demo" {
			t.Fatalf("precheck = %v", m)
		}
		if m := marker(t, o, "check_script"); m["status"] != "ok" || m["path"] != check {
			t.Fatalf("check_script = %v", m)
		}
		if m := marker(t, o, "shared"); m["status"] != "yes" || m["fstype"] != "nfs4" {
			t.Fatalf("shared = %v", m)
		}
		if m := marker(t, o, "distribute_summary"); m["skipped"] != "shared" || m["failed"] != "none" {
			t.Fatalf("distribute_summary = %v", m)
		}
		m := marker(t, o, "slurm_conf")
		if m["status"] != "ok" || m["path"] != w.confPath() || m["backup"] != bs[0] || m["chained"] != "none" || m["interval"] != "300" {
			t.Fatalf("slurm_conf = %v", m)
		}
		if marker(t, o, "configless")["value"] != "yes" || marker(t, o, "reconfigure")["status"] != "ok" || marker(t, o, "done")["status"] != "ok" {
			t.Fatalf("markers:\n%s", o)
		}
		if strings.Contains(o, "NOTICE") {
			t.Fatalf("unexpected notice:\n%s", o)
		}
	})
}

func TestSetupMarkersAreOnlyKeyValue(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.env["FAKE_FAIL_NODES"] = "cn001"
		r := w.runSetup()
		n := 0
		for _, line := range strings.Split(r.stdout, "\n") {
			if strings.HasPrefix(line, "GWKS_SLURM") {
				parseMarker(t, line)
				n++
			}
		}
		if n < 8 {
			t.Fatalf("only %d markers:\n%s", n, r.stdout)
		}
	})
}

func TestSetupNotRoot(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.env["FAKE_UID"] = "1000"
		r := w.runSetup()
		if r.code != 1 || !strings.Contains(r.stdout, "must run as root") {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if m := marker(t, r.stdout, "precheck"); m["status"] != "failed" || m["reason"] != "not_root" {
			t.Fatalf("precheck = %v", m)
		}
		if marker(t, r.stdout, "done")["status"] != "failed" {
			t.Fatalf("done:\n%s", r.stdout)
		}
		if w.read(w.confPath()) != baseConf || w.exists(w.p.ScriptDir) {
			t.Fatal("changed something without being root")
		}
		for _, mode := range []string{"--remove", "--uninstall-agents"} {
			if r := w.runSetup(mode); r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "not_root" {
				t.Fatalf("%s as non-root: exit %d\n%s", mode, r.code, r.stdout)
			}
		}
	})
}

func TestSetupRefusesWhenSlurmctldIsUnreachable(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.flag("scontrol-down", true)
		for _, args := range [][]string{nil, {"--remove"}, {"--uninstall-agents"}} {
			r := w.runSetup(args...)
			if r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "slurmctld_unreachable" {
				t.Fatalf("%v: exit %d\n%s", args, r.code, r.stdout)
			}
		}
		if w.read(w.confPath()) != baseConf || w.exists(w.p.ScriptDir) || len(backups(t, w)) != 0 {
			t.Fatal("changed something while slurmctld was unreachable")
		}
	})
}

func TestSetupRefusesWhenScontrolIsMissingOrConfIsUnreadable(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		_ = os.Remove(w.confPath())
		r := w.runSetup()
		if r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "conf_unreadable" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		w.write(w.confPath(), baseConf)
		w.remove("scontrol")
		r = w.runSetup()
		if r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "scontrol_not_found" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
	})
}

func TestSetupRejectsBadArguments(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		for _, args := range [][]string{{"--interval", "0"}, {"--interval", "abc"}, {"--interval", "-5"}, {"--interval"}, {"--interval=1.5"}, {"--bogus"}, {"--remove", "--uninstall-agents"}} {
			r := w.runSetup(args...)
			if r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "bad_arguments" {
				t.Fatalf("%v: exit %d\n%s", args, r.code, r.stdout)
			}
		}
		if w.read(w.confPath()) != baseConf || w.exists(w.p.ScriptDir) {
			t.Fatal("bad arguments changed something")
		}
	})
}

func TestSetupNotConfigless(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.env["FAKE_CTLD_PARAMS"] = "(null)"
		r := w.runSetup()
		if r.code != 0 || marker(t, r.stdout, "configless")["value"] != "no" || marker(t, r.stdout, "done")["status"] != "partial" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		for _, s := range []string{"NOTICE", "not configless", "scontrol reconfigure only tells slurmd to"} {
			if !strings.Contains(r.stdout, s) {
				t.Fatalf("missing %q in\n%s", s, r.stdout)
			}
		}
		if w.stateFile("reconfigured") == "" {
			t.Fatal("did not reconfigure")
		}
	})
}

func TestSetupConfiglessAmongOtherParameters(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.env["FAKE_CTLD_PARAMS"] = "cloud_dns,enable_configless,idle_on_node_suspend"
		r := w.runSetup()
		if marker(t, r.stdout, "configless")["value"] != "yes" || marker(t, r.stdout, "done")["status"] != "ok" {
			t.Fatalf("%s", r.stdout)
		}
	})
}

func TestSetupReconfigureFailure(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.flag("reconfigure-fail", true)
		r := w.runSetup()
		if r.code != 1 || marker(t, r.stdout, "reconfigure")["status"] != "failed" || marker(t, r.stdout, "done")["status"] != "failed" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if !strings.Contains(w.read(w.confPath()), "# BEGIN gpuworks") {
			t.Fatal("the conf edit was rolled back although only the reconfigure failed")
		}
	})
}

func TestSetupRefusesAnUnterminatedBlock(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		conf := baseConf + "# BEGIN gpuworks\nHealthCheckProgram=/x\n"
		w.controller("nfs", conf)
		r := w.runSetup()
		if r.code != 1 || marker(t, r.stdout, "precheck")["reason"] != "conf_unterminated_block" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if w.read(w.confPath()) != conf {
			t.Fatal("edited a conf it could not understand")
		}
	})
}

func TestSetupKeepsConfOwnershipModeAndInode(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		if err := os.Chmod(w.confPath(), 0o640); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(w.confPath())
		w.runSetup()
		after, _ := os.Stat(w.confPath())
		if !os.SameFile(before, after) || after.Mode().Perm() != 0o640 {
			t.Fatalf("inode or mode changed: %v -> %v", before.Mode(), after.Mode())
		}
		bs := backups(t, w)
		if fi, _ := os.Stat(bs[0]); fi.Mode().Perm() != 0o640 {
			t.Fatalf("backup mode = %v", fi.Mode().Perm())
		}
	})
}

func TestSetupWritesThroughASymlinkedConf(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		real := w.path("etc/real.conf")
		w.write(real, baseConf)
		_ = os.Remove(w.confPath())
		if err := os.Symlink(real, w.confPath()); err != nil {
			t.Fatal(err)
		}
		w.runSetup()
		if fi, _ := os.Lstat(w.confPath()); fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the symlink was replaced by a file")
		}
		if !strings.Contains(w.read(real), "# BEGIN gpuworks") {
			t.Fatal("the target was not edited")
		}
	})
}

func TestSetupRerunsAreIdempotent(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		conf := baseConf + "HealthCheckProgram=/usr/sbin/nhc\nHealthCheckInterval=120\n"
		w.controller("nfs", conf)
		w.runSetup()
		once := w.read(w.confPath())
		nbak := len(backups(t, w))
		chained := w.read(filepath.Join(w.p.ScriptDir, "chained"))
		// the chained file is lost: it is recovered from the original line
		_ = os.Remove(filepath.Join(w.p.ScriptDir, "chained"))
		r := w.runSetup()
		if got := w.read(w.confPath()); got != once {
			t.Fatalf("second run changed the file:\n%s\nwas:\n%s", got, once)
		}
		if n := len(backups(t, w)); n != nbak {
			t.Fatalf("backups %d -> %d although nothing changed", nbak, n)
		}
		if got := w.read(filepath.Join(w.p.ScriptDir, "chained")); got != chained || got != "/usr/sbin/nhc\nANY\n" {
			t.Fatalf("chained = %q", got)
		}
		if m := marker(t, r.stdout, "slurm_conf"); m["backup"] != "none" || m["chained"] != "/usr/sbin/nhc" || m["interval"] != "120" {
			t.Fatalf("slurm_conf = %v", m)
		}
		if strings.Count(w.read(w.confPath()), "# BEGIN gpuworks") != 1 {
			t.Fatal("more than one block")
		}
		if len(strings.Split(w.stateFile("reconfigured"), "\n")) != 3 {
			t.Fatalf("each run reconfigures: %q", w.stateFile("reconfigured"))
		}
	})
}

func TestSetupRerunAfterChangingTheCheckScriptRewritesIt(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		w.runSetup()
		check := filepath.Join(w.p.ScriptDir, "check.sh")
		w.write(check, "# stale\n")
		w.runSetup()
		if w.read(check) != CheckScript(w.p) {
			t.Fatal("the stale check script was not replaced")
		}
	})
}

func TestSetupRunsFromAPipeEvenWhenCommandsReadStdin(t *testing.T) {
	// The script arrives on stdin; ssh, scp and friends read stdin when they
	// can. A fake that swallows stdin shows that setup never lets them.
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.writeExec(filepath.Join(w.bin, "scp"), "#!/bin/sh\ncat >/dev/null\nexit 0\n")
		w.writeExec(filepath.Join(w.bin, "ssh"), "#!/bin/sh\ncat >/dev/null\necho GWKS_OK\nexit 0\n")
		r := w.runSetup()
		if r.code != 0 || marker(t, r.stdout, "done")["status"] != "ok" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
	})
}

// The token goes next to the check script in a root-only file, is read back
// before setup goes on, and never appears in the script itself.
func TestSetupWritesTheTokenRootOnlyAndReadsItBack(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		tok := filepath.Join(w.p.ScriptDir, "token")
		if got := w.read(tok); got != w.p.Token+"\n" {
			t.Fatalf("token file = %q", got)
		}
		if fi, _ := os.Stat(tok); fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
		}
		if strings.Contains(w.read(filepath.Join(w.p.ScriptDir, "check.sh")), w.p.Token) {
			t.Fatal("the installed check script carries the token")
		}
		if m := marker(t, r.stdout, "token_file"); m["status"] != "ok" || m["path"] != tok {
			t.Fatalf("token_file = %v", m)
		}
		if strings.Contains(r.stdout, w.p.Token) {
			t.Fatal("setup printed the token")
		}
		if left, _ := filepath.Glob(filepath.Join(w.p.ScriptDir, ".*")); len(left) != 0 {
			t.Fatalf("temporary files left: %v", left)
		}
	})
}

func TestSetupRefusesWhenTheTokenFileCannotBeRead(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		// A directory where the file goes: it can be neither replaced nor read as a token.
		if err := os.MkdirAll(filepath.Join(w.p.ScriptDir, "token"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := w.runSetup()
		if r.code != 1 {
			t.Fatalf("exit %d, want 1\n%s", r.code, r.stdout)
		}
		if m := marker(t, r.stdout, "token_file"); m["status"] != "failed" {
			t.Fatalf("token_file = %v", m)
		}
		if marker(t, r.stdout, "done")["status"] != "failed" {
			t.Fatalf("done marker:\n%s", r.stdout)
		}
		if len(markers(t, r.stdout, "slurm_conf")) != 0 || w.stateFile("reconfigured") != "" {
			t.Fatal("slurm.conf was changed although the nodes cannot enroll")
		}
	})
}

func TestRemoveDeletesTheTokenFile(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.runSetup()
		if !w.exists(filepath.Join(w.p.ScriptDir, "token")) {
			t.Fatal("no token file after setup")
		}
		if r := w.runSetup("--remove"); r.code != 0 {
			t.Fatalf("remove: exit %d\n%s", r.code, r.stdout)
		}
		if w.exists(filepath.Join(w.p.ScriptDir, "token")) {
			t.Fatal("the token file outlived --remove")
		}
	})
}
