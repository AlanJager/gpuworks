package cloudprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	vastdriver "gpuworks.ai/gpuworks/internal/server/provider/vast"
)

func TestVastRentChoosesCheapestBoundedOffer(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	privateKey, _, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeProviderDriver{
		result: serverprovider.Instance{ExternalID: "contract-101"},
		listed: []serverprovider.Instance{{
			ExternalID: "contract-101", Name: TestNamePrefix + "vast-run", GPUModel: "RTX 5070 Ti", GPUCount: 1,
			Power: serverprovider.PowerRunning, PublicIP: "203.0.113.8", SSHPort: 50226, SSHUser: "root", Region: "US",
			SSHConnections: []serverprovider.SSHConnection{
				{ID: "direct", Type: "direct", Host: "203.0.113.8", Port: 50226, User: "root", InstallSupported: true},
				{ID: "proxy", Type: "proxy", Host: "ssh8.vast.ai", Port: 41022, User: "root", InstallSupported: true},
			},
		}},
	}
	driver := &fakeVastDriver{fakeProviderDriver: base, offers: []vastdriver.Offer{
		{ID: json.Number("1"), GPUName: "H100 SXM", NumGPUs: 1, GPURAMMB: 81920, DPHTotal: 0.05},
		{ID: json.Number("2"), GPUName: "RTX 4090", NumGPUs: 1, GPURAMMB: 24576, DPHTotal: 0.21},
		{ID: json.Number("3"), GPUName: "RTX 5070 Ti", NumGPUs: 1, GPURAMMB: 16384, DPHTotal: 0.12, Reliability: 0.99},
		{ID: json.Number("4"), GPUName: "RTX 5070", NumGPUs: 1, GPURAMMB: 12288, DPHTotal: 0.30},
	}}
	p, err := newVast(driver, serverprovider.Credentials{APIKey: "key"}, "vast-run", privateKey, 0.25, "nvidia/cuda:test")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000, DiskGB: 30})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "contract-101" || instance.PublicIP != "203.0.113.8" || instance.SSHPort != 50226 || instance.GPUModel != "RTX 5070 Ti" || len(instance.PrivateKey) == 0 {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.search.MinGPURAMMB != 12000 || driver.search.DiskGB != 30 || base.created.Extra["offer_id"] != "3" || base.created.Image != "nvidia/cuda:test" {
		t.Fatalf("search=%+v create=%+v", driver.search, base.created)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(base.deleted) != 1 || base.deleted[0] != "contract-101" {
		t.Fatalf("deleted = %v", base.deleted)
	}
	if _, err := os.Stat(vastLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("vast ledger still exists: %v", err)
	}
}

func TestVastRentRejectsOffersOutsideSafetyBounds(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	privateKey, _, _ := GenerateSSHKey()
	base := &fakeProviderDriver{}
	driver := &fakeVastDriver{fakeProviderDriver: base, offers: []vastdriver.Offer{
		{ID: json.Number("1"), GPUName: "H100 SXM", NumGPUs: 1, GPURAMMB: 81920, DPHTotal: 0.05},
		{ID: json.Number("2"), GPUName: "RTX 4090", NumGPUs: 1, GPURAMMB: 24576, DPHTotal: 0.26},
		{ID: json.Number("3"), GPUName: "RTX 5070", NumGPUs: 1, GPURAMMB: 8000, DPHTotal: 0.08},
	}}
	p, _ := newVast(driver, serverprovider.Credentials{APIKey: "key"}, "run", privateKey, 0.25, "image")
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", MinVRAMMB: 12000}); err == nil || !strings.Contains(err.Error(), "no allowlisted offer") {
		t.Fatalf("Rent error = %v", err)
	}
	if base.created.Extra != nil {
		t.Fatalf("unsafe offer was created: %+v", base.created)
	}
}

func TestVastRentFailureDestroysAllocatedContract(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	privateKey, _, _ := GenerateSSHKey()
	base := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "contract-failed"}}
	driver := &fakeVastDriver{fakeProviderDriver: base, offers: []vastdriver.Offer{{
		ID: json.Number("7"), GPUName: "RTX 5070", NumGPUs: 1, GPURAMMB: 12288, DPHTotal: 0.09,
	}}}
	p, _ := newVast(driver, serverprovider.Credentials{APIKey: "key"}, "run", privateKey, 0.25, "image")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Rent(ctx, GPUSpec{GPUModel: "auto", MinVRAMMB: 12000}); err == nil || !strings.Contains(err.Error(), "wait for vast SSH endpoint") {
		t.Fatalf("Rent error = %v", err)
	}
	if len(base.deleted) != 1 || base.deleted[0] != "contract-failed" {
		t.Fatalf("failed contract cleanup = %v", base.deleted)
	}
}

func TestVastRentUsesProxyWhenDirectSSHIsUnavailable(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	privateKey, _, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeProviderDriver{
		result: serverprovider.Instance{ExternalID: "contract-proxy"},
		listed: []serverprovider.Instance{{
			ExternalID: "contract-proxy", Name: TestNamePrefix + "vast-run", GPUModel: "RTX 4090", GPUCount: 1,
			Power: serverprovider.PowerRunning, Region: "US",
			SSHConnections: []serverprovider.SSHConnection{{
				ID: "proxy", Type: "proxy", Host: "ssh8.vast.ai", Port: 41022, User: "root", InstallSupported: true,
			}},
		}},
	}
	driver := &fakeVastDriver{fakeProviderDriver: base, offers: []vastdriver.Offer{{
		ID: json.Number("8"), GPUName: "RTX 4090", NumGPUs: 1, GPURAMMB: 24576, DPHTotal: 0.12,
	}}}
	p, err := newVast(driver, serverprovider.Credentials{APIKey: "key"}, "vast-run", privateKey, 0.25, "image")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000})
	if err != nil {
		t.Fatal(err)
	}
	if instance.PublicIP != "ssh8.vast.ai" || instance.SSHPort != 41022 || instance.Username != "root" {
		t.Fatalf("proxy endpoint = %s@%s:%d", instance.Username, instance.PublicIP, instance.SSHPort)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
}

func TestVastRentDestroysSSHRejectingCandidateAndFallsBack(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	privateKey, _, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeProviderDriver{
		results: []serverprovider.Instance{{ExternalID: "contract-bad"}, {ExternalID: "contract-good"}},
		listed: []serverprovider.Instance{
			{
				ExternalID: "contract-bad", Name: TestNamePrefix + "vast-run", GPUModel: "RTX 5070 Ti", GPUCount: 1,
				Power:          serverprovider.PowerRunning,
				SSHConnections: []serverprovider.SSHConnection{{ID: "proxy-bad", Type: "proxy", Host: "ssh1.vast.ai", Port: 35894, User: "root", InstallSupported: true}},
			},
			{
				ExternalID: "contract-good", Name: TestNamePrefix + "vast-run", GPUModel: "RTX 4090", GPUCount: 1,
				Power:          serverprovider.PowerRunning,
				SSHConnections: []serverprovider.SSHConnection{{ID: "proxy-good", Type: "proxy", Host: "ssh2.vast.ai", Port: 35895, User: "root", InstallSupported: true}},
			},
		},
	}
	driver := &fakeVastDriver{fakeProviderDriver: base, offers: []vastdriver.Offer{
		{ID: json.Number("10"), GPUName: "RTX 5070 Ti", NumGPUs: 1, GPURAMMB: 16384, DPHTotal: 0.10, Reliability: 0.99},
		{ID: json.Number("11"), GPUName: "RTX 4090", NumGPUs: 1, GPURAMMB: 24576, DPHTotal: 0.12, Reliability: 0.99},
	}}
	p, err := newVast(driver, serverprovider.Credentials{APIKey: "key"}, "vast-run", privateKey, 0.25, "image")
	if err != nil {
		t.Fatal(err)
	}
	readyCalls := 0
	p.sshReady = func(context.Context, string, int, string, []byte) error {
		readyCalls++
		if readyCalls == 1 {
			return errors.New("ssh: unable to authenticate")
		}
		return nil
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000})
	if err != nil {
		t.Fatal(err)
	}
	if readyCalls != 2 || len(base.createdGPUs) != 2 || base.createdGPUs[0] != "RTX 5070 Ti" || base.createdGPUs[1] != "RTX 4090" {
		t.Fatalf("SSH calls=%d GPU attempts=%v", readyCalls, base.createdGPUs)
	}
	if instance.InstanceID != "contract-good" || len(base.deleted) != 1 || base.deleted[0] != "contract-bad" {
		t.Fatalf("failed candidate cleanup=%v", base.deleted)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
}

func TestVastListLeakedCombinesCloudLabelsAndLedger(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	privateKey, _, _ := GenerateSSHKey()
	if err := addVastLedger("ledger-only"); err != nil {
		t.Fatal(err)
	}
	base := &fakeProviderDriver{listed: []serverprovider.Instance{
		{ExternalID: "production", Name: "production"},
		{ExternalID: "cloud-leak", Name: TestNamePrefix + "old"},
	}}
	p, _ := newVast(&fakeVastDriver{fakeProviderDriver: base}, serverprovider.Credentials{APIKey: "key"}, "run", privateKey, 0.25, "image")
	leaked, err := p.ListLeaked(t.Context())
	if err != nil || len(leaked) != 2 || leaked[0] != "cloud-leak" || leaked[1] != "ledger-only" {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}

func TestVastListLeakedForRunIDIsIsolated(t *testing.T) {
	privateKey, _, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	base := &fakeProviderDriver{listed: scopedCleanupInventory()}
	p, err := newVast(&fakeVastDriver{fakeProviderDriver: base}, serverprovider.Credentials{APIKey: "key"}, "gitlab-1-20", privateKey, 0.25, "image")
	if err != nil {
		t.Fatal(err)
	}
	assertScopedCleanup(t, p)
}
