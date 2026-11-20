package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const reachTestServer = "https://gw.example:8800"

// A setup that printed which hosts cannot reach the server finishes partial,
// reads the hosts, and does not take the failed host for a failed step.
func TestParseSlurmApplyReadsTheHostsThatCannotReachTheServer(t *testing.T) {
	out := "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=x\nGWKS_SLURM step=shared status=yes fstype=nfs4\n" +
		"GWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared\n" +
		"GWKS_SLURM step=server_reach role=controller host=ctl status=ok\n" +
		"GWKS_SLURM step=server_reach role=node host=gpu000 status=failed reason=timeout\n" +
		"GWKS_SLURM step=server_reach role=node host=gpu001 status=unknown reason=not_asked\n" +
		"GWKS_SLURM step=server_reach_summary url=https://gw.example:8800 ok=1 failed=gpu000 unknown=gpu001\n" +
		"GWKS_SLURM step=slurm_conf status=ok path=x\nGWKS_SLURM step=configless value=yes\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=partial\n"
	res, err := ParseSlurmApply(SlurmModeSetup, out, "")
	if err != nil {
		t.Fatalf("a host that cannot reach the server is no failed step: %v", err)
	}
	want := []SlurmReachHost{
		{Role: "controller", Name: "ctl", Status: "ok"},
		{Role: "node", Name: "gpu000", Status: "failed", Reason: "timeout"},
		{Role: "node", Name: "gpu001", Status: "unknown", Reason: "not_asked"},
	}
	if !reflect.DeepEqual(res.Reach.Hosts, want) || !res.Partial() {
		t.Fatalf("reach = %+v, partial %v", res.Reach, res.Partial())
	}
}

// A check carries the same warning code the plan shows, and a run whose hosts
// all reached the server carries none.
func TestPlanWarnsWhenAHostCannotReachTheServer(t *testing.T) {
	res := SlurmApplyResult{Plan: &SlurmApplyPlan{Configless: true}}
	if w := res.PlanWarnings(); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
	res.SetReach(SlurmReach{Hosts: []SlurmReachHost{{Role: "node", Name: "gpu000", Status: "unknown", Reason: "not_asked"}}})
	if w := res.PlanWarnings(); len(w) != 0 || res.Partial() {
		t.Fatalf("a node that was not asked is no warning: %v", w)
	}
	res.SetReach(SlurmReach{Hosts: []SlurmReachHost{{Role: "node", Name: "gpu000", Status: "failed", Reason: "dns"}}})
	if w := res.PlanWarnings(); !reflect.DeepEqual(w, []string{SlurmPlanWarnServerUnreach}) || !res.Partial() {
		t.Fatalf("warnings = %v, partial %v", w, res.Partial())
	}
}

// The step of an apply that says how reaching the server came out.
func TestSetReachAddsTheStepOfASetup(t *testing.T) {
	step := func(hosts ...SlurmReachHost) []SlurmApplyStep {
		res := SlurmApplyResult{Mode: SlurmModeSetup}
		res.SetReach(SlurmReach{Hosts: hosts})
		return res.Steps
	}
	if got := step(); len(got) != 0 {
		t.Fatalf("no host asked: steps = %v", got)
	}
	ok := SlurmReachHost{Role: "controller", Name: "ctl", Status: "ok"}
	failed := SlurmReachHost{Role: "node", Name: "gpu000", Status: "failed", Reason: "connect"}
	unknown := SlurmReachHost{Role: "node", Name: "gpu001", Status: "unknown", Reason: "not_asked"}
	for _, tc := range []struct {
		hosts []SlurmReachHost
		want  string
	}{{[]SlurmReachHost{ok}, "pass"}, {[]SlurmReachHost{ok, failed}, "warn"}, {[]SlurmReachHost{unknown}, "skip"}, {[]SlurmReachHost{ok, unknown}, "pass"}} {
		if got := step(tc.hosts...); !reflect.DeepEqual(got, []SlurmApplyStep{{SlurmApplyIDReach, tc.want}}) {
			t.Errorf("%v: steps = %v, want reach %s", tc.hosts, got, tc.want)
		}
	}
	check := SlurmApplyResult{Mode: SlurmModeCheck}
	check.SetReach(SlurmReach{Hosts: []SlurmReachHost{failed}})
	if len(check.Steps) != 0 {
		t.Fatalf("a check has no steps: %v", check.Steps)
	}
}

func TestMergeReachKeepsOneAnswerPerHostAndTheWorstOfTwo(t *testing.T) {
	script := []SlurmReachHost{
		{Role: "node", Name: "gpu001", Status: "ok"},
		{Role: "controller", Name: "ctl", Status: "ok"},
		{Role: "node", Name: "gpu002", Status: "unknown", Reason: "not_asked"},
		{Role: "node", Name: "gpu003", Status: "ok"},
	}
	runs := []SlurmNodeRun{
		{Name: "gpu000.cluster.local", Output: "GWKS_REACH rc=28\n"},
		{Name: "gpu001", Output: "GWKS_REACH rc=7\n"},
		{Name: "gpu002", Output: "GWKS_REACH rc=0\n"},
		{Name: "gpu003", NotAsked: SlurmReachAssistantOffline},
		{Name: "gpu004", NotAsked: SlurmReachNodeNotRunning},
	}
	got := mergeReach(script, runs, "", "").Hosts
	want := []SlurmReachHost{
		{Role: "controller", Name: "ctl", Status: "ok"},
		{Role: "node", Name: "gpu000.cluster.local", Status: "failed", Reason: "timeout"},
		{Role: "node", Name: "gpu001", Status: "failed", Reason: "connect"},
		{Role: "node", Name: "gpu002", Status: "ok"},
		{Role: "node", Name: "gpu003", Status: "ok"},
		{Role: "node", Name: "gpu004", Status: "unknown", Reason: "node_not_running"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged\n got %+v\nwant %+v", got, want)
	}
}

func TestReachOfNodeRunReadsTheStatusTheScriptPrinted(t *testing.T) {
	for _, tc := range []struct {
		run  SlurmNodeRun
		want SlurmReachHost
	}{
		{SlurmNodeRun{Name: "n", Output: "GWKS_REACH rc=0\n"}, SlurmReachHost{Role: "node", Name: "n", Status: "ok"}},
		{SlurmNodeRun{Name: "n", Output: "noise\nGWKS_REACH rc=6\n"}, SlurmReachHost{Role: "node", Name: "n", Status: "failed", Reason: "dns"}},
		{SlurmNodeRun{Name: "n", Output: "GWKS_REACH rc=127\n"}, SlurmReachHost{Role: "node", Name: "n", Status: "failed", Reason: "no_curl"}},
		{SlurmNodeRun{Name: "n", Output: "GWKS_REACH rc=x\n"}, SlurmReachHost{Role: "node", Name: "n", Status: "unknown", Reason: "command_failed"}},
		{SlurmNodeRun{Name: "n", Output: "sh: curl: boom\n"}, SlurmReachHost{Role: "node", Name: "n", Status: "unknown", Reason: "command_failed"}},
		{SlurmNodeRun{Name: "n", NotAsked: "assistant_offline", Output: "GWKS_REACH rc=0\n"}, SlurmReachHost{Role: "node", Name: "n", Status: "unknown", Reason: "assistant_offline"}},
	} {
		if got := reachOfNodeRun(tc.run); got != tc.want {
			t.Errorf("%+v = %+v, want %+v", tc.run, got, tc.want)
		}
	}
}

// curl's exit statuses are worded the same way here as in the setup script.
var curlReasons = map[int]string{0: "", 127: "no_curl", 6: "dns", 7: "connect", 28: "timeout", 22: "http", 35: "tls", 60: "tls", 51: "tls", 1: "other", 56: "other"}

func TestSlurmReachReasonWordsCurlsExitStatuses(t *testing.T) {
	for rc, want := range curlReasons {
		if got := slurmReachReason(rc); got != want {
			t.Errorf("rc %d = %q, want %q", rc, got, want)
		}
	}
}

// The script the platform runs on a compute node, in the shells it may meet:
// it prints curl's own exit status, and 127 when there is no curl.
func TestReachProbeScriptPrintsCurlsExitStatus(t *testing.T) {
	script, err := SlurmReachProbeScript(reachTestServer + "/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "url='"+reachTestServer+"/healthz'") {
		t.Fatalf("script does not ask /healthz of the server:\n%s", script)
	}
	eachShell(t, func(t *testing.T, shell string) {
		for rc := range curlReasons {
			if rc == 127 {
				continue
			}
			dir := t.TempDir()
			curl := "#!/bin/sh\necho \"$*\" >> \"$FAKE_DIR/curl.args\"\nexit " + itoa(rc) + "\n"
			if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o755); err != nil {
				t.Fatal(err)
			}
			out := runProbe(t, shell, script, dir, dir)
			if want := "GWKS_REACH rc=" + itoa(rc) + "\n"; out != want {
				t.Fatalf("rc %d: output %q, want %q", rc, out, want)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "curl.args")); string(got) != "-fsS --max-time 10 -o /dev/null "+reachTestServer+"/healthz\n" {
				t.Fatalf("curl was run as %q", got)
			}
		}
		empty := t.TempDir()
		if out := runProbe(t, shell, script, empty, empty); out != "GWKS_REACH rc=127\n" {
			t.Fatalf("no curl: output %q", out)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

func runProbe(t *testing.T, shell, script, binDir, stateDir string) string {
	t.Helper()
	for _, tool := range []string{"sh", "bash", "dash"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(stateDir, "shell-"+tool))
		}
	}
	cmd := exec.Command(shell, "-c", script)
	cmd.Env = []string{"PATH=" + binDir, "FAKE_DIR=" + stateDir, "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	return string(out)
}

func TestReachProbeScriptRefusesAnUnsafeURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "https://x/'; reboot", "https://x/a b", "https://x/$(id)", "https:///nohost"} {
		if _, err := SlurmReachProbeScript(u); err == nil {
			t.Errorf("%q was accepted", u)
		}
	}
	req := setupReq()
	req.ServerURL = "https://x/'; reboot"
	if req.Validate() == nil {
		t.Fatal("a request with an unsafe server URL was accepted")
	}
	req.ServerURL = reachTestServer
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The manager's setup is told not to ask the nodes when the platform does,
// and asks them (the script's own default) otherwise.
func TestManagerScriptSkipsAskingNodesOnlyWhenThePlatformDoes(t *testing.T) {
	for _, mode := range []string{SlurmModeSetup, SlurmModeCheck} {
		for _, skip := range []bool{false, true} {
			eachShell(t, func(t *testing.T, shell string) {
				m := newManager(t, "gpu000")
				if mode == SlurmModeCheck {
					m.setup(0, "GWKS_SLURM step=done status=ok mode=check")
				}
				req := SlurmApplyRequest{Mode: mode, SetupURL: applyTestURL, SkipNodeReach: skip}
				m.run(shell, req)
				want := map[bool]string{false: "", true: "--no-node-reach"}[skip]
				if mode == SlurmModeCheck {
					want = strings.TrimSpace("--check " + want)
				}
				if got := strings.TrimSpace(strings.TrimPrefix(m.read("setup.args"), "args=")); got != want {
					t.Fatalf("%s skip=%v: setup arguments %q, want %q", mode, skip, got, want)
				}
			})
		}
	}
}

// proberServiceDriver is the apply account whose driver also asks the nodes.
type proberServiceDriver struct {
	*applyServiceDriver
	nodes   []SlurmNodeRun
	err     error
	scripts []string
	asked   chan struct{}
}

func (d *proberServiceDriver) RunOnSlurmNodes(ctx context.Context, _ Credentials, region, clusterID, script string, _ time.Duration) ([]SlurmNodeRun, error) {
	d.scripts = append(d.scripts, region+"/"+clusterID+" "+script)
	if d.asked != nil {
		close(d.asked)
	}
	return d.nodes, d.err
}

func newProberService(t *testing.T, name string) (*Service, SlurmApplyTarget, *proberServiceDriver) {
	t.Helper()
	inner := &applyServiceDriver{
		hpcTestDriver: hpcTestDriver{channelTestDriver: channelTestDriver{fallbackTestDriver: fallbackTestDriver{name: name}}},
		instances:     []Instance{computeNode("i-0", "ehpc-sz-1_gpu000", "ehpc-sz-1", "slurm", 2)},
		answer: func(string) (SlurmManagerRun, error) {
			return SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=x\n" +
				"GWKS_SLURM step=shared status=yes fstype=nfs4\nGWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared\nGWKS_SLURM step=slurm_conf status=ok path=x\n" +
				"GWKS_SLURM step=server_reach role=controller host=ctl status=ok\nGWKS_SLURM step=configless value=yes\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\n"}, nil
		},
	}
	drv := &proberServiceDriver{applyServiceDriver: inner}
	svc, providerID := newFallbackService(t, &inner.fallbackTestDriver)
	Register(drv)
	if _, err := svc.Sync(t.Context(), providerID); err != nil {
		t.Fatal(err)
	}
	target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	return svc, target, drv
}

// The platform asks the nodes while the manager runs, tells the manager's
// script not to, and reads both answers as one.
func TestRunSlurmSetupAsksTheNodesItselfAndMergesTheAnswers(t *testing.T) {
	svc, target, drv := newProberService(t, "apply-prober")
	drv.nodes = []SlurmNodeRun{{Name: "gpu000", InstanceID: "i-0", Output: "GWKS_REACH rc=28\n"}}
	req := SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL, ServerURL: reachTestServer}
	res, err := svc.RunSlurmSetup(t.Context(), target, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []SlurmReachHost{{Role: "controller", Name: "ctl", Status: "ok"}, {Role: "node", Name: "gpu000", Status: "failed", Reason: "timeout"}}
	if !reflect.DeepEqual(res.Reach.Hosts, want) || !res.Partial() {
		t.Fatalf("reach = %+v, partial %v", res.Reach, res.Partial())
	}
	if last := res.Steps[len(res.Steps)-1]; last != (SlurmApplyStep{SlurmApplyIDReach, "warn"}) {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if len(drv.runs) == 0 || !strings.Contains(drv.runs[len(drv.runs)-1], "reach_flag='--no-node-reach'") {
		t.Fatalf("the manager was not told to skip the nodes: %q", drv.runs)
	}
	if len(drv.scripts) == 0 || !strings.HasPrefix(drv.scripts[len(drv.scripts)-1], "cn-shenzhen/ehpc-sz-1 ") || !strings.Contains(drv.scripts[len(drv.scripts)-1], reachTestServer+"/healthz") {
		t.Fatalf("nodes asked with %q", drv.scripts)
	}
}

// Without a server URL, or for a removal, the platform asks no node.
func TestRunSlurmSetupAsksNoNodeForARemovalOrWithoutAServerURL(t *testing.T) {
	svc, target, drv := newProberService(t, "apply-prober-none")
	drv.answer = func(string) (SlurmManagerRun, error) {
		return SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_SLURM step=remove status=ok path=x backup=none\nGWKS_SLURM step=done status=ok\n"}, nil
	}
	if _, err := svc.RunSlurmSetup(t.Context(), target, SlurmApplyRequest{Mode: SlurmModeRemoveCheck, SetupURL: applyTestURL, ServerURL: reachTestServer}); err != nil {
		t.Fatal(err)
	}
	drv.answer = func(string) (SlurmManagerRun, error) {
		return SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=x\nGWKS_SLURM step=slurm_conf status=ok path=x\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\n"}, nil
	}
	res, err := svc.RunSlurmSetup(t.Context(), target, SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL})
	if err != nil {
		t.Fatal(err)
	}
	if len(drv.scripts) != 0 || len(res.Reach.Hosts) != 0 || strings.Contains(drv.runs[len(drv.runs)-1], "--no-node-reach'") {
		t.Fatalf("nodes asked %q, reach %+v, manager %q", drv.scripts, res.Reach, drv.runs)
	}
}

// Nodes the cloud will not list are said, not hidden and not fatal: the key
// lacks the permission, or the listing failed.
func TestRunSlurmSetupSaysWhyTheNodesWereNotAsked(t *testing.T) {
	svc, target, drv := newProberService(t, "apply-prober-unlisted")
	req := SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL, ServerURL: reachTestServer}
	drv.err = &SlurmApplyPermissionError{Permission: "ehpc:ListNodes", Err: errors.New("denied")}
	res, err := svc.RunSlurmSetup(t.Context(), target, req)
	if err != nil || res.Reach.NodesUnlisted != "permission_missing" || res.Reach.NodesPermission != "ehpc:ListNodes" || len(res.Reach.Hosts) != 1 {
		t.Fatalf("permission: %+v, %v", res.Reach, err)
	}
	drv.err = errors.New("timeout talking to the cloud")
	res, err = svc.RunSlurmSetup(t.Context(), target, req)
	if err != nil || res.Reach.NodesUnlisted != "failed" || res.Reach.NodesPermission != "" {
		t.Fatalf("failure: %+v, %v", res.Reach, err)
	}
	// The manager failing is the run's failure, whatever the nodes said.
	drv.err = nil
	drv.answer = func(string) (SlurmManagerRun, error) { return SlurmManagerRun{}, ErrSlurmApplyManagerUnknown }
	if _, err := svc.RunSlurmSetup(t.Context(), target, req); !errors.Is(err, ErrSlurmApplyManagerUnknown) {
		t.Fatalf("manager failure = %v", err)
	}
}
