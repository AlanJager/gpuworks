package cloudprovider

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	autodldriver "gpuworks.ai/gpuworks/internal/server/provider/autodl"
)

type fakeAutoDLDriver struct {
	created     serverprovider.CreateInstanceRequest
	create      serverprovider.Instance
	snapshot    autodldriver.Snapshot
	listed      []serverprovider.Instance
	deleted     []string
	deleteErr   error
	snapshotErr error
}

func (d *fakeAutoDLDriver) ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error) {
	return d.listed, nil
}

func (d *fakeAutoDLDriver) CreateInstance(_ context.Context, _ serverprovider.Credentials, req serverprovider.CreateInstanceRequest) (serverprovider.Instance, error) {
	d.created = req
	return d.create, nil
}

func (d *fakeAutoDLDriver) DeleteInstance(_ context.Context, _ serverprovider.Credentials, id string) error {
	d.deleted = append(d.deleted, id)
	return d.deleteErr
}

func (d *fakeAutoDLDriver) Snapshot(context.Context, serverprovider.Credentials, string) (autodldriver.Snapshot, error) {
	return d.snapshot, d.snapshotErr
}

func TestAutoDLRentReturnsSnapshotSSHAndCleansUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{
		create: serverprovider.Instance{ExternalID: "pro-test"},
		snapshot: autodldriver.Snapshot{Instance: serverprovider.Instance{
			ExternalID: "pro-test", GPUModel: "NVIDIA RTX 4080S", GPUCount: 1, PricePerHr: 1.97,
			Region: "westDC3", PublicIP: "connect.example", SSHPort: 34222, SSHUser: "root",
		}, RootPassword: "password"},
	}
	p, err := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000, DiskGB: 20})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "pro-test" || instance.PublicIP != "connect.example" || instance.SSHPort != 34222 || instance.Password != "password" || instance.Kind != "container" {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.created.Name != TestNamePrefix+"run" || driver.created.GPUModel != autodlDefaultGPUModel || driver.created.Image != "base-image" || driver.created.Extra["expand_system_disk_by_gb"] != "20" {
		t.Fatalf("create request = %+v", driver.created)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(driver.deleted) != 1 || driver.deleted[0] != "pro-test" {
		t.Fatalf("deleted = %v", driver.deleted)
	}
	if _, err := os.Stat(autodlLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("ledger still exists: %v", err)
	}
}

func TestAutoDLRentDestroysInstanceAbovePriceLimit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{
		create: serverprovider.Instance{ExternalID: "pro-expensive"},
		snapshot: autodldriver.Snapshot{Instance: serverprovider.Instance{
			ExternalID: "pro-expensive", PricePerHr: 5.01, PublicIP: "connect.example", SSHPort: 34222,
		}, RootPassword: "password"},
	}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX 4080S"}); err == nil || !strings.Contains(err.Error(), "exceeds configured maximum") {
		t.Fatalf("Rent error = %v", err)
	}
	if len(driver.deleted) != 1 || driver.deleted[0] != "pro-expensive" {
		t.Fatalf("cleanup = %v", driver.deleted)
	}
}

func TestAutoDLListLeakedIncludesLedger(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := addAutoDLLedger("ledger-only"); err != nil {
		t.Fatal(err)
	}
	driver := &fakeAutoDLDriver{listed: []serverprovider.Instance{
		{ExternalID: "keep", Name: "production"}, {ExternalID: "cloud-leak", Name: TestNamePrefix + "old"},
	}}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	leaked, err := p.ListLeaked(t.Context())
	if err != nil || len(leaked) != 2 || leaked[0] != "cloud-leak" || leaked[1] != "ledger-only" {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}

func TestAutoDLListLeakedForRunIDIsIsolated(t *testing.T) {
	driver := &fakeAutoDLDriver{listed: scopedCleanupInventory()}
	p, err := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "gitlab-1-20", "base-image", 5)
	if err != nil {
		t.Fatal(err)
	}
	assertScopedCleanup(t, p)
}

func TestAutoDLDestroyKeepsLedgerOnFailure(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := addAutoDLLedger("pro-failed"); err != nil {
		t.Fatal(err)
	}
	driver := &fakeAutoDLDriver{deleteErr: errors.New("release failed")}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if err := p.Destroy(t.Context(), "pro-failed"); err == nil {
		t.Fatal("Destroy succeeded unexpectedly")
	}
	ledger, err := readAutoDLLedger()
	if err != nil || len(ledger) != 1 || ledger[0] != "pro-failed" {
		t.Fatalf("ledger=%v err=%v", ledger, err)
	}
}
