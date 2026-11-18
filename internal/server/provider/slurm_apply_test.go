package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// applyServiceDriver is an account holding E-HPC compute nodes (named
// "<cluster>_<node>") whose manager node runs what it is sent.
type applyServiceDriver struct {
	hpcTestDriver
	instances []Instance
	runs      []string
	region    string
	timeout   time.Duration
	answer    func(script string) (SlurmManagerRun, error)
}

func (d *applyServiceDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return d.instances, nil
}

func (d *applyServiceDriver) RunOnSlurmManager(_ context.Context, _ Credentials, region, clusterID, script string, timeout time.Duration) (SlurmManagerRun, error) {
	d.runs = append(d.runs, clusterID+"@"+region+": "+script)
	d.region, d.timeout = region, timeout
	return d.answer(script)
}

func computeNode(id, name, cluster, scheduler string, gpus int) Instance {
	return Instance{ExternalID: id, Name: name, Region: "cn-shenzhen", GPUCount: gpus, Kind: "vm", Power: PowerRunning,
		HPC: &HPCCluster{ID: cluster, Scheduler: scheduler}}
}

func newApplyService(t *testing.T, name string, instances ...Instance) (*Service, uint, *applyServiceDriver) {
	t.Helper()
	driver := &applyServiceDriver{
		hpcTestDriver: hpcTestDriver{channelTestDriver: channelTestDriver{fallbackTestDriver: fallbackTestDriver{name: name}}},
		instances:     instances,
		answer: func(string) (SlurmManagerRun, error) {
			return SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=x\n" +
				"GWKS_SLURM step=shared status=yes fstype=nfs4\nGWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared\nGWKS_SLURM step=slurm_conf status=ok path=x\n" +
				"GWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\n"}, nil
		},
	}
	svc, providerID := newFallbackService(t, &driver.fallbackTestDriver)
	Register(driver)
	if _, err := svc.Sync(t.Context(), providerID); err != nil {
		t.Fatal(err)
	}
	return svc, providerID, driver
}

func notApplicableReason(t *testing.T, err error) string {
	t.Helper()
	var na *SlurmApplyNotApplicableError
	if !errors.As(err, &na) || !errors.Is(err, ErrSlurmApplyNotApplicable) {
		t.Fatalf("err = %v, want SlurmApplyNotApplicableError", err)
	}
	return na.Reason
}

func TestSlurmApplyTargetIsTheClustersGPUComputeNodesBySlurmName(t *testing.T) {
	svc, providerID, _ := newApplyService(t, "apply-target",
		computeNode("i-2", "ehpc-sz-1_gpu002", "ehpc-sz-1", "slurm", 8),
		computeNode("i-0", "ehpc-sz-1_gpu000", "ehpc-sz-1", "slurm", 1),
		computeNode("i-cpu", "ehpc-sz-1_cpu000", "ehpc-sz-1", "slurm", 0),
		computeNode("i-odd", "renamed-by-hand", "ehpc-sz-1", "slurm", 1),
		computeNode("i-evil", "ehpc-sz-1_gpu;reboot", "ehpc-sz-1", "slurm", 1),
		computeNode("i-pbs", "ehpc-sz-2_comp000", "ehpc-sz-2", "pbs", 1),
	)
	target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if target.ProviderID != providerID || target.ClusterID != "ehpc-sz-1" || target.Region != "cn-shenzhen" || strings.Join(target.ExpectGPU, ",") != "gpu000,gpu002" {
		t.Fatalf("target = %+v, want nodes gpu000,gpu002", target)
	}
}

func TestSlurmApplyTargetSaysWhyNot(t *testing.T) {
	svc, providerID, _ := newApplyService(t, "apply-not-applicable",
		computeNode("i-0", "ehpc-sz-1_gpu000", "ehpc-sz-1", "slurm", 1),
		computeNode("i-pbs", "ehpc-sz-2_comp000", "ehpc-sz-2", "pbs", 1),
		computeNode("i-unread", "ehpc-sz-3_gpu000", "ehpc-sz-3", "", 1),
	)
	for _, tc := range []struct {
		name    string
		id      uint
		cluster string
		reason  string
	}{
		{"not Slurm", providerID, "ehpc-sz-2", messages.SlurmApplyReasonNotSlurmCluster},
		{"scheduler not read", providerID, "ehpc-sz-3", messages.SlurmApplyReasonNotSlurmCluster},
		{"no node synced", providerID, "ehpc-sz-9", messages.SlurmApplyReasonClusterUnknown},
		{"no such account", 9999, "ehpc-sz-1", messages.SlurmApplyReasonNoProvider},
	} {
		_, err := svc.SlurmApplyTarget(t.Context(), tc.id, tc.cluster, 0)
		if got := notApplicableReason(t, err); got != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, got, tc.reason)
		}
	}
	// A driver that cannot run commands on the manager.
	plain, plainID, _ := newHPCService(t, "apply-plain-driver")
	if _, err := plain.SlurmApplyTarget(t.Context(), plainID, "ehpc-sz-1", 0); notApplicableReason(t, err) != messages.SlurmApplyReasonProviderUnsupported {
		t.Fatalf("plain driver err = %v", err)
	}
	if plain.CanRunSlurmSetup("apply-plain-driver") || !svc.CanRunSlurmSetup("apply-not-applicable") || svc.CanRunSlurmSetup("no-such-kind") {
		t.Fatal("CanRunSlurmSetup does not follow the driver")
	}
}

func TestRunSlurmSetupSendsTheScriptAndReadsTheOutcome(t *testing.T) {
	svc, providerID, driver := newApplyService(t, "apply-run", computeNode("i-0", "ehpc-sz-1_gpu000", "ehpc-sz-1", "slurm", 2))
	target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.RunSlurmSetup(t.Context(), target, SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL, ExpectGPU: target.ExpectGPU})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManagerInstance != "i-mgr" || res.Done != "ok" || res.Partial() || len(res.Steps) != 6 {
		t.Fatalf("result = %+v", res)
	}
	if len(driver.runs) != 1 || !strings.HasPrefix(driver.runs[0], "ehpc-sz-1@cn-shenzhen: ") || !strings.Contains(driver.runs[0], "expect_gpu='gpu000'") || driver.timeout != SlurmApplyTimeout {
		t.Fatalf("runs = %q, timeout %v", driver.runs, driver.timeout)
	}

	// A request the script could not carry out as written never reaches the manager.
	if _, err := svc.RunSlurmSetup(t.Context(), target, SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: "https://x/'; reboot", ExpectGPU: target.ExpectGPU}); err == nil || len(driver.runs) != 1 {
		t.Fatalf("an unsafe URL was run: %v, runs %d", err, len(driver.runs))
	}

	// What the driver fails with, and what the script printed, come back as typed errors.
	driver.answer = func(string) (SlurmManagerRun, error) { return SlurmManagerRun{}, ErrSlurmApplyManagerUnknown }
	if _, err := svc.RunSlurmSetup(t.Context(), target, SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL}); !errors.Is(err, ErrSlurmApplyManagerUnknown) {
		t.Fatalf("manager unknown = %v", err)
	}
	driver.answer = func(string) (SlurmManagerRun, error) {
		return SlurmManagerRun{ManagerInstance: "i-mgr", Output: "boom\n", Ended: "the command ended Failed with exit code 1"}, nil
	}
	var failed *SlurmApplyFailedError
	if _, err := svc.RunSlurmSetup(t.Context(), target, SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL}); !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepRun || !strings.Contains(failed.Output, "exit code 1") {
		t.Fatalf("failed script = %v", err)
	}

	// A script the manager node could not fetch names the server address it
	// could not reach (the entry's ServerURL, else the setup URL without its
	// path and one-time code) and why.
	driver.answer = func(string) (SlurmManagerRun, error) {
		return SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_APPLY fetch=failed rc=28\n"}, nil
	}
	for _, tc := range []struct{ name, serverURL, want string }{
		{"entry's server url", "http://203.0.113.9:8800/", "http://203.0.113.9:8800"},
		{"derived from the setup url", "", "https://x:8800"},
	} {
		req := SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: "https://x:8800/slurm/setup.sh?once=abc123", ServerURL: tc.serverURL}
		if _, err := svc.RunSlurmSetup(t.Context(), target, req); !errors.As(err, &failed) || failed.Reason != "server_unreachable" || failed.Kind != "timeout" || failed.URL != tc.want || strings.Contains(err.Error(), "abc123") {
			t.Fatalf("%s: unreachable = %#v", tc.name, err)
		}
	}

	// An account gone since the target was found.
	if _, err := svc.RunSlurmSetup(t.Context(), SlurmApplyTarget{ProviderID: 9999, ClusterID: "ehpc-sz-1", Region: "cn-shenzhen"}, SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL}); err == nil {
		t.Fatal("ran on a missing account")
	}
}
