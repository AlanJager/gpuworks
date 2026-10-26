package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

const (
	t4Small = "ecs.gn6i-c8g1.2xlarge"
	t4Dual  = "ecs.gn6i-c24g1.12xlarge"
)

func newTestAliyun(t *testing.T, f *fakeECS, mutate func(*AliyunConfig)) *Aliyun {
	t.Helper()
	cfg := AliyunConfig{
		AccessKeyID: "LTAI-test-id", AccessKeySecret: "test-secret-never-logged", Endpoint: f.srv.URL,
		Images: map[string]string{"cn-alpha": "m-alpha", "cn-beta": "m-beta"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := newAliyun(aliyundriver.NewRPC(), cfg, "run1")
	if err != nil {
		t.Fatal(err)
	}
	p.poll = time.Millisecond
	return p
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAliyunSelectPlacementListsSpotBeforeOnDemand(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockClosedWithStock, "cn-alpha-b|"+t4Small)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small, "cn-alpha-c|"+t4Small)
	f.stock("cn-beta", false, stockWithStock, "cn-beta-a|"+t4Small)
	p := newTestAliyun(t, f, nil)

	best, err := p.SelectPlacement(testCtx(t), "T4", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !best.Spot || best.Zone != "cn-alpha-a" || best.Stock != stockWithStock {
		t.Fatalf("best placement = %v stock=%s, want spot in cn-alpha-a with plain stock", best, best.Stock)
	}
	var order []string
	for _, c := range p.candidates[aliyunPlacementKey("T4", 1)] {
		order = append(order, c.String())
	}
	want := []string{
		"cn-alpha/cn-alpha-a " + t4Small + " spot", "cn-alpha/cn-alpha-b " + t4Small + " spot",
		"cn-alpha/cn-alpha-a " + t4Small + " on-demand", "cn-alpha/cn-alpha-c " + t4Small + " on-demand",
		"cn-beta/cn-beta-a " + t4Small + " on-demand",
	}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("candidate order\n got %v\nwant %v", order, want)
	}
	// The sold-out row the API lists must never become a candidate, and a
	// shape with another GPU count must not match.
	if _, err := p.SelectPlacement(testCtx(t), "T4", 2); err == nil || !strings.Contains(err.Error(), "no aliyun capacity for T4 x2") {
		t.Fatalf("T4 x2 without stock error = %v", err)
	}
}

func TestAliyunSelectPlacementReportsStockQueryErrors(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
	f.regions = []string{"cn-alpha"} // cn-beta is not a region of this account
	p := newTestAliyun(t, f, nil)
	best, err := p.SelectPlacement(testCtx(t), "T4", 1)
	if err != nil || best.Region != "cn-alpha" {
		t.Fatalf("a region that cannot be queried must not hide a region that can: %v %v", best, err)
	}
	f.mu.Lock()
	f.odStock = map[string]map[string]string{}
	f.mu.Unlock()
	p.stockCache = map[string]aliyunStockEntry{}
	_, err = p.SelectPlacement(testCtx(t), "T4", 1)
	if err == nil || !strings.Contains(err.Error(), "InvalidRegionId.NotFound") {
		t.Fatalf("no capacity must name the stock query errors, got %v", err)
	}
}

func TestAliyunRentBuysSpotAndCreatesTheSharedNetworkOnce(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
	p := newTestAliyun(t, f, nil)

	first, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if first.Billing != "spot" || first.InstanceType != t4Small || first.Zone != "cn-alpha-a" || first.Region != "cn-alpha" ||
		first.Kind != "vm" || first.Username != "root" || first.SSHPort != 22 || first.GPUModel != "T4" || first.GPUCount != 1 {
		t.Fatalf("rented instance = %+v", first)
	}
	if first.PublicIP == "" || first.PrivateIP == "" || !validPassword(first.Password) {
		t.Fatalf("instance has no usable addresses or password: %+v", first)
	}
	runs := f.callsOf("RunInstances")
	if len(runs) != 1 {
		t.Fatalf("RunInstances calls = %d, want 1", len(runs))
	}
	q := runs[0].Params
	for key, want := range map[string]string{
		"ImageId": "m-alpha", "InstanceType": t4Small, "ZoneId": "cn-alpha-a", "InstanceChargeType": "PostPaid",
		"SpotStrategy": "SpotAsPriceGo", "SpotInterruptionBehavior": "Terminate", "InstanceName": "gpuworks-test-run1",
		"SystemDisk.Size": "160", "SystemDisk.Category": "cloud_essd", "InternetMaxBandwidthOut": "100",
		"Tag.1.Key": "gpuworks-test-run", "Tag.1.Value": "run1",
	} {
		if q.Get(key) != want {
			t.Fatalf("RunInstances %s = %q, want %q (all: %v)", key, q.Get(key), want, q)
		}
	}
	if strings.Contains(fmt.Sprint(p.Attempts()), first.Password) {
		t.Fatal("the attempt log must never carry the root password")
	}

	// Infra: one VPC, one security group with SSH and agent port rules (members of one normal group reach each other by default).
	if len(f.vpcs) != 1 || len(f.groups) != 1 || len(f.vswitches) != 1 {
		t.Fatalf("infra vpcs=%d groups=%d vswitches=%d, want 1 each", len(f.vpcs), len(f.groups), len(f.vswitches))
	}
	var group *fakeGroup
	for _, g := range f.groups {
		group = g
	}
	if group.Name != "gpuworks-test-infra" || group.Tags["gpuworks-test-infra"] != "1" {
		t.Fatalf("shared group = %+v", group)
	}
	have := map[string]bool{}
	for _, r := range group.Rules {
		have[r["IpProtocol"]+" "+r["PortRange"]+" cidr="+r["SourceCidrIp"]+" group="+(map[bool]string{true: "self", false: ""}[r["SourceGroupId"] == group.ID])] = true
	}
	for _, want := range []string{"tcp 22/22 cidr=0.0.0.0/0 group=", "tcp 18800/18800 cidr=0.0.0.0/0 group="} {
		if !have[want] {
			t.Fatalf("security group lacks rule %q; has %v", want, have)
		}
	}

	// A second VM of the same shape reuses the network (no further creates)
	// and the pinned zone.
	second, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if second.Zone != first.Zone || second.InstanceType != first.InstanceType {
		t.Fatalf("second VM left the first VM's zone/type: %+v vs %+v", second, first)
	}
	for _, action := range []string{"CreateVpc", "CreateVSwitch", "CreateSecurityGroup", "AuthorizeSecurityGroup"} {
		want := map[string]int{"CreateVpc": 1, "CreateVSwitch": 1, "CreateSecurityGroup": 1, "AuthorizeSecurityGroup": 2}[action]
		if got := len(f.callsOf(action)); got != want {
			t.Fatalf("%s calls = %d, want %d", action, got, want)
		}
	}
}

func TestAliyunRentReusesExistingNetworkAndRules(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	// A previous run (another process) already built the network.
	if _, err := newTestAliyun(t, f, nil).Rent(testCtx(t), GPUSpec{GPUModel: "T4"}); err != nil {
		t.Fatal(err)
	}
	creates := len(f.callsOf("CreateVpc")) + len(f.callsOf("CreateVSwitch")) + len(f.callsOf("CreateSecurityGroup")) + len(f.callsOf("AuthorizeSecurityGroup"))
	fresh := newTestAliyun(t, f, nil) // empty in-process cache
	if _, err := fresh.Rent(testCtx(t), GPUSpec{GPUModel: "T4"}); err != nil {
		t.Fatal(err)
	}
	after := len(f.callsOf("CreateVpc")) + len(f.callsOf("CreateVSwitch")) + len(f.callsOf("CreateSecurityGroup")) + len(f.callsOf("AuthorizeSecurityGroup"))
	if after != creates {
		t.Fatalf("a second process re-created shared infra: %d create/authorize calls before, %d after", creates, after)
	}
}

func TestAliyunRentFallsBackToOnDemandWhenSpotIsRefused(t *testing.T) {
	cases := map[string]struct {
		code, msg string
		status    int
	}{
		"stock changed between check and create": {"OperationDenied.NoStock", "The requested resource is sold out in the specified zone.", 400},
		"spot vCPU quota":                        {"QuotaExceed.ElasticQuota", "The number of vCPUs exceeds the quota.", 400},
		"spot not sold for this type":            {"InvalidResourceType.NotSupported", "The specified resource type is not supported.", 400},
	}
	for name, refusal := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeECS(t)
			// The stock snapshot says spot is available; the create then refuses.
			f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
			f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
			f.runHook = func(_ *fakeECS, q url.Values) (string, string, int) {
				if q.Get("SpotStrategy") != "" {
					return refusal.code, refusal.msg, refusal.status
				}
				return "", "", 0
			}
			p := newTestAliyun(t, f, nil)
			instance, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
			if err != nil {
				t.Fatal(err)
			}
			if instance.Billing != "on-demand" {
				t.Fatalf("billing = %q, want on-demand after a spot refusal", instance.Billing)
			}
			runs := f.callsOf("RunInstances")
			if len(runs) != 2 || runs[0].Params.Get("SpotStrategy") != "SpotAsPriceGo" || runs[1].Params.Get("SpotStrategy") != "" {
				t.Fatalf("RunInstances sequence = %d calls, want spot then on-demand", len(runs))
			}
			if !strings.Contains(strings.Join(p.Attempts(), "\n"), refusal.code) {
				t.Fatalf("attempt log must show why spot was refused: %v", p.Attempts())
			}
		})
	}
}

func TestAliyunRentDoesNotSwallowAccountErrors(t *testing.T) {
	for _, code := range []string{"InvalidAccountStatus.NotEnoughBalance", "InvalidAccessKeyId.NotFound", "Forbidden.RAM"} {
		t.Run(code, func(t *testing.T) {
			f := newFakeECS(t)
			f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
			f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
			f.runHook = func(*fakeECS, url.Values) (string, string, int) { return code, "account problem", 403 }
			p := newTestAliyun(t, f, nil)
			_, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
			if err == nil || aliyundriver.ErrorCode(err) != code {
				t.Fatalf("Rent error = %v, want the %s API error", err, code)
			}
			if got := len(f.callsOf("RunInstances")); got != 1 {
				t.Fatalf("RunInstances calls = %d: an account problem must not be retried across zones and billing modes", got)
			}
			if len(f.liveInstances()) != 0 {
				t.Fatal("a refused create left an instance")
			}
		})
	}
}

func TestAliyunRentWithNoStockReportsCapacityAndBuysNothing(t *testing.T) {
	f := newFakeECS(t)
	p := newTestAliyun(t, f, nil)
	_, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "L20", GPUCount: 2})
	if err == nil || !strings.Contains(err.Error(), "no aliyun capacity") {
		t.Fatalf("Rent without stock = %v, want a no-capacity error the L4 harness treats as a stock-out", err)
	}
	if len(f.callsOf("RunInstances")) != 0 || len(f.callsOf("CreateVpc")) != 0 {
		t.Fatal("nothing may be created when no stock exists")
	}
}

func TestAliyunRentSkipsShapesBelowMinVRAM(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	p := newTestAliyun(t, f, nil)
	_, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, MinVRAMMB: 20000})
	if err == nil || !strings.Contains(err.Error(), "below the requested 20000 MiB") {
		t.Fatalf("T4 (16 GiB) for a 20000 MiB case: %v", err)
	}
}

func TestAliyunRentTriesNextCardWhenTheFirstHasNoStock(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-beta", true, stockWithStock, "cn-beta-a|ecs.gn8is.2xlarge")
	p := newTestAliyun(t, f, nil)
	instance, err := p.Rent(testCtx(t), GPUSpec{GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if instance.GPUModel != "L20" || instance.Region != "cn-beta" || instance.Billing != "spot" {
		t.Fatalf("with only L20 in stock an unpinned Rent must land on it: %+v", instance)
	}
	if runs := f.callsOf("RunInstances"); runs[0].Params.Get("ImageId") != "m-beta" {
		t.Fatalf("a VM in cn-beta must boot the cn-beta copy of the golden image, got %q", runs[0].Params.Get("ImageId"))
	}
}

func TestAliyunTwoVMRentalStaysInOneZoneAndFallsBackThere(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Dual, "cn-alpha-b|"+t4Dual)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Dual, "cn-alpha-b|"+t4Dual)
	spotCreates := 0
	f.runHook = func(_ *fakeECS, q url.Values) (string, string, int) {
		if q.Get("SpotStrategy") == "" {
			return "", "", 0
		}
		spotCreates++
		if spotCreates > 1 {
			return "OperationDenied.NoStock", "sold out", 400 // the last spot machine went to someone else
		}
		return "", "", 0
	}
	p := newTestAliyun(t, f, nil)
	if _, err := p.SelectPlacement(testCtx(t), "T4", 2); err != nil {
		t.Fatal(err)
	}
	first, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 2, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 2, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if first.Billing != "spot" || second.Billing != "on-demand" {
		t.Fatalf("billing = %s then %s, want spot then on-demand fallback", first.Billing, second.Billing)
	}
	if first.Zone != second.Zone || first.InstanceType != second.InstanceType {
		t.Fatalf("two-VM rental split across %s/%s and %s/%s: they would not share a private network", first.Zone, first.InstanceType, second.Zone, second.InstanceType)
	}
	if first.GPUCount != 2 {
		t.Fatalf("GPU count = %d", first.GPUCount)
	}
}

func TestAliyunRentReplacesAnInstanceThatVanishesWhileBooting(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
	f.vanishAfter[1] = 1 // the first (spot) machine is reclaimed during boot
	p := newTestAliyun(t, f, nil)
	p.bootBy = 10 * time.Second
	instance, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if instance.Billing != "on-demand" {
		t.Fatalf("after the spot machine vanished the replacement must be on-demand, got %s", instance.Billing)
	}
	if len(f.liveInstances()) != 1 {
		t.Fatalf("live instances = %d, want only the replacement", len(f.liveInstances()))
	}
}

func TestAliyunRentReleasesAnInstanceThatNeverBoots(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
	f.bootAfter = 1 << 30 // Pending forever
	p := newTestAliyun(t, f, nil)
	p.bootBy = 50 * time.Millisecond
	_, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err == nil || !errors.Is(err, errAliyunBootFailed) && !strings.Contains(err.Error(), "did not become reachable") {
		t.Fatalf("Rent of a machine that never runs = %v", err)
	}
	// Destroy was asked for and the fake completes it within a few describes.
	deadline := time.Now().Add(5 * time.Second)
	for len(f.liveInstances()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.callsOf("DeleteInstance")) == 0 {
		t.Fatal("an instance that never booted was not released")
	}
}

func TestAliyunDestroyForcesDeleteWaitsAndIsIdempotent(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	p := newTestAliyun(t, f, nil)
	instance, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(testCtx(t), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(f.liveInstances()) != 0 || len(f.disks) != 0 {
		t.Fatalf("after Destroy instances=%d disks=%d, want none", len(f.liveInstances()), len(f.disks))
	}
	deletes := f.callsOf("DeleteInstance")
	if len(deletes) != 1 || deletes[0].Params.Get("Force") != "true" {
		t.Fatalf("DeleteInstance calls = %+v, want one with Force=true", deletes)
	}
	if err := p.Destroy(testCtx(t), instance.InstanceID); err != nil {
		t.Fatalf("destroying an already released instance must succeed: %v", err)
	}
}

func TestAliyunDestroyWaitsOutAnInstanceThatIsStillInitializing(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	p := newTestAliyun(t, f, nil)
	instance, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.refuseDeletes = 12 // more than any fixed retry cap
	f.mu.Unlock()
	if err := p.Destroy(testCtx(t), instance.InstanceID); err != nil {
		t.Fatalf("Destroy must keep trying while the instance initializes: %v", err)
	}
	if got := len(f.callsOf("DeleteInstance")); got != 13 {
		t.Fatalf("DeleteInstance calls = %d, want 12 refusals and one success", got)
	}
	if len(f.liveInstances()) != 0 {
		t.Fatal("the instance was not released")
	}
	// An error that is not a status conflict is reported at once.
	other, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4", GPUCount: 1, DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	f.runHook = nil
	cancelled, cancel := context.WithCancel(testCtx(t))
	f.mu.Lock()
	f.refuseDeletes = 1 << 20
	f.mu.Unlock()
	cancel()
	if err := p.Destroy(cancelled, other.InstanceID); err == nil {
		t.Fatal("a cancelled context must end the retries with an error")
	}
}

func TestAliyunDestroyFindsInstancesOfEarlierProcesses(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-beta", true, stockWithStock, "cn-beta-a|"+t4Small)
	owner := newTestAliyun(t, f, nil)
	instance, err := owner.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	// A cleanup process knows no region for the ID and has no images either.
	cleaner := newTestAliyun(t, f, func(c *AliyunConfig) { c.Images = nil })
	if err := cleaner.Destroy(testCtx(t), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(f.liveInstances()) != 0 {
		t.Fatal("cleanup by ID did not release the instance")
	}
}

func TestAliyunLeakListingIsScopedToTheRun(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	mine := newTestAliyun(t, f, nil)
	a, err := mine.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := newAliyun(aliyundriver.NewRPC(), AliyunConfig{AccessKeyID: "id", AccessKeySecret: "secret", Endpoint: f.srv.URL, Images: map[string]string{"cn-alpha": "m-alpha"}}, "run10")
	if err != nil {
		t.Fatal(err)
	}
	other.poll = time.Millisecond
	b, err := other.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	// A foreign instance of the account (no test prefix) must stay invisible.
	f.mu.Lock()
	f.instances["i-foreign"] = &fakeInstance{ID: "i-foreign", Name: "ecs-t6-prod", Region: "cn-alpha", ordinal: 99, Tags: map[string]string{}}
	f.mu.Unlock()

	cleanup := newTestAliyun(t, f, func(c *AliyunConfig) { c.Images = nil })
	got, err := cleanup.ListLeakedForRunID(testCtx(t), "run1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a.InstanceID {
		t.Fatalf("run1 leak list = %v, want only %s (run10's %s shares the name prefix)", got, a.InstanceID, b.InstanceID)
	}
	all, err := cleanup.ListLeaked(testCtx(t))
	if err != nil || len(all) != 2 {
		t.Fatalf("all test leaks = %v err=%v, want the two test instances and not the foreign one", all, err)
	}
	if _, err := cleanup.ListLeakedForRunID(testCtx(t), " "); err == nil {
		t.Fatal("an empty run ID must be refused: it would match every run")
	}
}

func TestAliyunLeakScanToleratesRegionsTheKeyCannotList(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	rent := newTestAliyun(t, f, nil)
	instance, err := rent.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	f.listBroken["us-gamma"] = "Forbidden.Region"
	cleanup := newTestAliyun(t, f, func(c *AliyunConfig) { c.Images = nil })
	got, err := cleanup.ListLeakedForRunID(testCtx(t), "run1")
	if err != nil || len(got) != 1 || got[0] != instance.InstanceID {
		t.Fatalf("derived-region scan with one unusable region = %v, %v", got, err)
	}
	// With explicit regions a failure is the operator's problem and must show.
	explicit := newTestAliyun(t, f, func(c *AliyunConfig) { c.Images = nil; c.Regions = []string{"us-gamma"} })
	if _, err := explicit.ListLeakedForRunID(testCtx(t), "run1"); err == nil {
		t.Fatal("an explicitly configured region that cannot be listed must fail the scan")
	}
	for _, region := range f.regions {
		f.listBroken[region] = "Forbidden.Region"
	}
	if _, err := cleanup.ListLeakedForRunID(testCtx(t), "run1"); err == nil {
		t.Fatal("when no region can be listed the scan must fail, not report a clean account")
	}
}

func TestAliyunLeakScanRetriesARegionThatFailsOnce(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	rent := newTestAliyun(t, f, nil)
	instance, err := rent.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	// The region the leak sits in answers badly twice before it answers. Even
	// a scan that tolerates unusable regions must not lose the leak to that.
	f.mu.Lock()
	f.listFlaky["cn-alpha"] = 2
	f.mu.Unlock()
	cleanup := newTestAliyun(t, f, func(c *AliyunConfig) { c.Images = nil })
	got, err := cleanup.ListLeakedForRunID(testCtx(t), "run1")
	if err != nil || len(got) != 1 || got[0] != instance.InstanceID {
		t.Fatalf("scan with a flaky region = %v, %v; the leak must still be found", got, err)
	}
}

func TestAliyunOrphanDisksAreReportedAndDeleted(t *testing.T) {
	f := newFakeECS(t)
	f.leakDisks = true
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	p := newTestAliyun(t, f, nil)
	instance, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(testCtx(t), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	disks, err := p.ListOrphanDisksForRunID(testCtx(t), "run1")
	if err != nil || len(disks) != 1 || !strings.HasPrefix(disks[0], "cn-alpha/d-") {
		t.Fatalf("orphan disks = %v err=%v", disks, err)
	}
	if err := p.DeleteOrphanDisk(testCtx(t), disks[0]); err != nil {
		t.Fatal(err)
	}
	if left, _ := p.ListOrphanDisksForRunID(testCtx(t), "run1"); len(left) != 0 {
		t.Fatalf("disks left after delete: %v", left)
	}
	if err := p.DeleteOrphanDisk(testCtx(t), "nonsense"); err == nil {
		t.Fatal("a malformed disk reference must be refused")
	}
}

func TestAliyunInterruptedRecognisesReclaimedSpotInstances(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	p := newTestAliyun(t, f, nil)
	instance, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	if gone, why := p.Interrupted(testCtx(t), instance.InstanceID); gone {
		t.Fatalf("a healthy instance reported interrupted: %s", why)
	}
	f.mu.Lock()
	f.instances[instance.InstanceID].recycling = true
	f.mu.Unlock()
	if gone, why := p.Interrupted(testCtx(t), instance.InstanceID); !gone || !strings.Contains(why, "Recycling") {
		t.Fatalf("Recycling lock not recognised: %v %q", gone, why)
	}
	f.mu.Lock()
	delete(f.instances, instance.InstanceID)
	f.mu.Unlock()
	if gone, why := p.Interrupted(testCtx(t), instance.InstanceID); !gone || !strings.Contains(why, "no longer exists") {
		t.Fatalf("vanished instance not recognised: %v %q", gone, why)
	}
	var _ InterruptionChecker = p
}

func TestAliyunNoSpotAndNoOnDemandRestrictBillingModes(t *testing.T) {
	f := newFakeECS(t)
	f.stock("cn-alpha", true, stockWithStock, "cn-alpha-a|"+t4Small)
	f.stock("cn-alpha", false, stockWithStock, "cn-alpha-b|"+t4Small)
	onDemand := newTestAliyun(t, f, func(c *AliyunConfig) { c.NoSpot = true })
	got, err := onDemand.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil || got.Billing != "on-demand" || got.Zone != "cn-alpha-b" {
		t.Fatalf("NoSpot rental = %+v err=%v", got, err)
	}
	spotOnly := newTestAliyun(t, f, func(c *AliyunConfig) { c.NoOnDemand = true })
	got, err = spotOnly.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
	if err != nil || got.Billing != "spot" {
		t.Fatalf("NoOnDemand rental = %+v err=%v", got, err)
	}
}

func TestAliyunRentWithoutGoldenImageFailsBeforeAnyAPICall(t *testing.T) {
	f := newFakeECS(t)
	p := newTestAliyun(t, f, func(c *AliyunConfig) { c.Images = nil })
	if _, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4"}); err == nil || !strings.Contains(err.Error(), "ALIYUN_IMAGE_") {
		t.Fatalf("Rent without an image = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("API calls made without an image: %d", len(f.calls))
	}
}

func TestAliyunManagementCredentialsLimitTheProductSyncToRentRegions(t *testing.T) {
	f := newFakeECS(t)
	p := newTestAliyun(t, f, nil)
	creds := p.ManagementCredentials()
	if creds.PublicKey != "LTAI-test-id" || creds.PrivateKey == "" || creds.Extra["regions"] != "cn-alpha,cn-beta" {
		t.Fatalf("management credentials = %+v", creds)
	}
}

func TestAliyunShapeTable(t *testing.T) {
	seen := map[string]bool{}
	for _, shape := range aliyunShapes {
		if seen[shape.InstanceType] {
			t.Fatalf("duplicate shape %s", shape.InstanceType)
		}
		seen[shape.InstanceType] = true
		if !strings.HasPrefix(shape.InstanceType, "ecs.gn") || shape.GPUCount < 1 || shape.VRAMMB < 10240 {
			t.Fatalf("shape %+v is not a GPU type with the memory the L4 cases need", shape)
		}
		if _, ok := aliyunShapeByType(shape.InstanceType); !ok {
			t.Fatalf("shape %s cannot be looked up", shape.InstanceType)
		}
	}
	if AliyunTensorParallel("T4", 2) || !AliyunTensorParallel("T4", 1) || !AliyunTensorParallel("L20", 2) || !AliyunTensorParallel("A10", 2) {
		t.Fatal("2xT4 must be excluded from tensor-parallel cases and every other shape kept")
	}
	if got := AliyunGPUModels(); strings.Join(got, ",") != "T4,A10,L20" {
		t.Fatalf("card order = %v, want cheapest first", got)
	}
	for _, c := range []struct {
		in, want string
		ok       bool
	}{{"L20", "L20", true}, {"NVIDIA L20", "L20", true}, {"Tesla T4", "T4", true}, {"a10", "A10", true}, {"RTX4090", "", false}} {
		if got, ok := canonicalAliyunModel(c.in); got != c.want || ok != c.ok {
			t.Fatalf("canonicalAliyunModel(%q) = %q,%v", c.in, got, ok)
		}
	}
}

func TestAliyunCapacityErrorClassification(t *testing.T) {
	// Shapes of real error bodies: the classification is by API error code.
	capacity := []string{"OperationDenied.NoStock", "OperationDenied.SpotInstanceTypeNotSupported", "QuotaExceed.ElasticQuota", "InvalidResourceType.NotSupported", "Zone.NotOnSale", "InvalidInstanceType.ValueNotSupported"}
	fatal := []string{"InvalidAccountStatus.NotEnoughBalance", "InvalidAccessKeyId.NotFound", "InvalidImageId.NotFound", "InvalidPassword.Malformed", "Forbidden.RAM", "SignatureDoesNotMatch"}
	f := newFakeECS(t)
	for _, group := range []struct {
		codes []string
		want  bool
	}{{capacity, true}, {fatal, false}} {
		for _, code := range group.codes {
			f.runHook = func(*fakeECS, url.Values) (string, string, int) { return code, "x", 400 }
			f.stock("cn-alpha", false, stockWithStock, "cn-alpha-a|"+t4Small)
			p := newTestAliyun(t, f, nil)
			_, err := p.Rent(testCtx(t), GPUSpec{GPUModel: "T4"})
			if err == nil {
				t.Fatalf("%s: rental succeeded", code)
			}
			if got := strings.Contains(err.Error(), "no aliyun capacity"); got != group.want {
				t.Fatalf("%s: capacity-class = %v, want %v (%v)", code, got, group.want, err)
			}
		}
	}
}

func TestAliyunGeneratedPasswordMeetsTheECSPolicy(t *testing.T) {
	for range 50 {
		password, err := generatePassword()
		if err != nil {
			t.Fatal(err)
		}
		if !validPassword(password) {
			t.Fatalf("generated password %q violates the ECS password policy", password)
		}
	}
}

func TestAliyunHostNameIsAValidLinuxHostName(t *testing.T) {
	for in, want := range map[string]string{
		"gpuworks-test-gitlab-12-345":              "gpuworks-test-gitlab-12-345",
		"gpuworks-test-" + strings.Repeat("a", 40): "gpuworks-test-aaaaaaaaaaaaaaaa",
		"__":     "gpuworks-test",
		"Run_ID": "runid",
	} {
		if got := hostNameFor(in); got != want || len(got) > 30 {
			t.Fatalf("hostNameFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAliyunFreeVSwitchCIDRAvoidsTakenRanges(t *testing.T) {
	taken := []aliyunVSwitch{{CIDRBlock: "172.30.9.0/24"}, {CIDRBlock: "172.30.10.0/24"}}
	got, err := freeVSwitchCIDR("cn-hangzhou-i", taken) // 'i' hints 9
	if err != nil || got != "172.30.11.0/24" {
		t.Fatalf("freeVSwitchCIDR = %q, %v", got, err)
	}
	if got, _ := freeVSwitchCIDR("cn-hangzhou-b", nil); got != "172.30.2.0/24" {
		t.Fatalf("zone b = %q, want a stable range derived from the zone letter", got)
	}
}
