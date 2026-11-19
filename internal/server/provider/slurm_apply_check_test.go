package provider

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// checkPlanLines are what the setup script prints for --check on a cluster
// that has its own health check and is not on shared storage.
var checkPlanLines = []string{
	"GWKS_SLURM step=precheck status=ok cluster=demo",
	"GWKS_SLURM step=check_script status=plan path=/opt/gpuworks-slurm/check.sh action=write",
	"GWKS_SLURM step=token_file status=plan path=/opt/gpuworks-slurm/token",
	"GWKS_SLURM step=chained status=plan program=/usr/sbin/nhc node_state=IDLE,CYCLE",
	"GWKS_SLURM step=install_mode status=plan rearm=yes",
	"GWKS_SLURM step=shared status=no fstype=ext4",
	"GWKS_SLURM step=distribute_plan nodes=gpu000,gpu001 files=check.sh,token,chained",
	"GWKS_SLURM step=conf_comment line=7 key=HealthCheckProgram value=/usr/sbin/nhc",
	"GWKS_SLURM step=conf_comment line=8 key=HealthCheckInterval value=60",
	"GWKS_SLURM step=slurm_conf status=plan path=/etc/slurm/slurm.conf chained=/usr/sbin/nhc interval=60 node_state=ANY,CYCLE changed=yes",
	"GWKS_SLURM step=configless value=yes",
	"GWKS_SLURM step=reconfigure status=plan",
	"GWKS_SLURM step=done status=ok mode=check",
}

func checkReq(expectGPU ...string) SlurmApplyRequest {
	return SlurmApplyRequest{Mode: SlurmModeCheck, SetupURL: applyTestURL, ExpectGPU: expectGPU}
}

// A check changes nothing on the manager node: no gres.conf line is written
// (the lines it would add are said), the setup script runs with --check, no
// node is resumed (the ones it would resume are said), and it is what the
// parser turns into a plan.
func TestSlurmApplyCheckWritesNothingAndReportsThePlan(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000", "gpu001=gpu:4", "gpu002=cpu:1")
		m.file("reasons", "gpu000="+gresReason+"\n")
		m.setup(0, checkPlanLines...)
		out := m.run(shell, checkReq("gpu000", "gpu001", "gpu002", "gpu009"))
		res, err := ParseSlurmApply(SlurmModeCheck, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if m.conf() != "<missing>" {
			t.Fatalf("a check wrote gres.conf: %q", m.conf())
		}
		if calls := m.read("scontrol.log"); strings.Contains(calls, "update") || strings.Contains(calls, "reconfigure") {
			t.Fatalf("a check changed Slurm: %q", calls)
		}
		if got := m.read("setup.args"); got != "args=--check\n" {
			t.Fatalf("setup arguments = %q, want --check", got)
		}
		plan := res.Plan
		if plan == nil || len(res.Added) != 0 || len(res.Resumed) != 0 {
			t.Fatalf("a check reported changes made: %+v", res)
		}
		if len(plan.GresAdd) != 2 || plan.GresAdd[0] != (SlurmGresLine{"gpu000", "/dev/nvidia0"}) || plan.GresAdd[1] != (SlurmGresLine{"gpu001", "/dev/nvidia[0-3]"}) {
			t.Fatalf("gres lines it would add = %+v", plan.GresAdd)
		}
		if strings.Join(res.NoGPUGres, ",") != "gpu002" {
			t.Fatalf("no gpu gres %v", res.NoGPUGres)
		}
		if strings.Join(plan.Resume, ",") != "gpu000" {
			t.Fatalf("it would resume %v", plan.Resume)
		}
		want := SlurmApplyPlan{
			SlurmCluster: "demo", GresAdd: plan.GresAdd, CheckScript: "/opt/gpuworks-slurm/check.sh", TokenFile: "/opt/gpuworks-slurm/token",
			FSType: "ext4", CopyTo: []string{"gpu000", "gpu001"}, CopyFiles: []string{"check.sh", "token", "chained"},
			ConfPath: "/etc/slurm/slurm.conf", ConfChanged: true, Interval: 60, NodeState: "ANY,CYCLE",
			ChainedProgram: "/usr/sbin/nhc", ChainedState: "IDLE,CYCLE", Configless: true, Rearm: true, Resume: plan.Resume,
			Commented: []SlurmConfComment{{7, "HealthCheckProgram", "/usr/sbin/nhc"}, {8, "HealthCheckInterval", "60"}},
		}
		if got := *plan; !samePlan(got, want) {
			t.Fatalf("plan\n got %+v\nwant %+v", got, want)
		}
		if w := res.PlanWarnings(); strings.Join(w, ",") != "no_gpu_gres" || res.Done != "ok" {
			t.Fatalf("warnings = %v", w)
		}
		// The same cluster in setup mode does write: the difference is the mode.
		real := newManager(t, "gpu000")
		_ = real.run(shell, setupReq())
		if real.conf() == "<missing>" {
			t.Fatal("setup did not write gres.conf")
		}
	})
}

func samePlan(a, b SlurmApplyPlan) bool {
	return a.SlurmCluster == b.SlurmCluster && a.CheckScript == b.CheckScript && a.TokenFile == b.TokenFile && a.Shared == b.Shared && a.FSType == b.FSType &&
		strings.Join(a.CopyTo, ",") == strings.Join(b.CopyTo, ",") && strings.Join(a.CopyFiles, ",") == strings.Join(b.CopyFiles, ",") &&
		a.ConfPath == b.ConfPath && a.ConfChanged == b.ConfChanged && a.Interval == b.Interval && a.NodeState == b.NodeState &&
		a.ChainedProgram == b.ChainedProgram && a.ChainedState == b.ChainedState && a.Configless == b.Configless && a.Rearm == b.Rearm &&
		len(a.Commented) == len(b.Commented) && a.Commented[0] == b.Commented[0] && a.Commented[1] == b.Commented[1] &&
		strings.Join(a.Includes, ",") == strings.Join(b.Includes, ",") && a.UnnamedProgram == b.UnnamedProgram
}

// A check whose plan has warnings from the script says them, and a check that
// stopped, did not print the plan or the precheck refused fails like a setup.
func TestParseSlurmApplyCheckWarningsAndFailures(t *testing.T) {
	warn := append(append([]string{}, checkPlanLines[:6]...),
		"GWKS_SLURM step=conf_include status=warn files=/etc/slurm/a.conf,/etc/slurm/b.conf",
		"GWKS_SLURM step=conf_source status=warn reason=program_not_in_conf program=/usr/sbin/nhc",
		"GWKS_SLURM step=distribute_plan nodes=none files=check.sh,token skipped=shared",
		"GWKS_SLURM step=slurm_conf status=plan path=/etc/slurm/slurm.conf chained=none interval=300 node_state=ANY changed=no",
		"GWKS_SLURM step=configless value=no", "GWKS_SLURM step=reconfigure status=plan", "GWKS_SLURM step=done status=partial mode=check")
	res, err := ParseSlurmApply(SlurmModeCheck, strings.Join(warn, "\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.PlanWarnings(), ","); got != "conf_include,program_not_in_conf,not_configless" || !res.Partial() {
		t.Fatalf("warnings = %q partial %v", got, res.Partial())
	}
	if strings.Join(res.Plan.Includes, ",") != "/etc/slurm/a.conf,/etc/slurm/b.conf" || res.Plan.UnnamedProgram != "/usr/sbin/nhc" || res.Plan.ConfChanged || res.Plan.Configless {
		t.Fatalf("plan = %+v", res.Plan)
	}
	if len(res.Plan.CopyTo) != 0 {
		t.Fatalf("a shared directory copies to %v", res.Plan.CopyTo)
	}

	for name, c := range map[string]struct{ out, reason, step string }{
		"stopped before the plan was finished": {strings.Join(checkPlanLines[:7], "\n"), "", messages.SlurmApplyStepRun},
		"a setup mode's output":                {"GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=done status=ok", "", messages.SlurmApplyStepRun},
		"the precheck refused":                 {"ERROR: not root\nGWKS_SLURM step=precheck status=failed reason=not_root\nGWKS_SLURM step=done status=failed", "not_root", messages.SlurmApplyStepSetup},
		"nothing printed":                      {"", "", messages.SlurmApplyStepRun},
		"another run is going":                 {"GWKS_APPLY busy=1", "busy", messages.SlurmApplyStepRun},
	} {
		_, err := ParseSlurmApply(SlurmModeCheck, c.out, "")
		var failed *SlurmApplyFailedError
		if !errors.As(err, &failed) || failed.Step != c.step || failed.Reason != c.reason {
			t.Errorf("%s: err = %v, want step %s reason %q", name, err, c.step, c.reason)
		}
	}
}
