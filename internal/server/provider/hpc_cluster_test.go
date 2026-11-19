package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// hpcTestDriver lists, like an Alibaba Cloud account holding E-HPC
// clusters, one compute node per scheduler and a plain instance; it records
// what reaches the vendor.
type hpcTestDriver struct {
	channelTestDriver
	calls []string
}

func (d *hpcTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	node := func(id string, hpc *HPCCluster) Instance {
		return Instance{ExternalID: id, Name: id, Region: "cn-shenzhen", PrivateIP: "127.0.0.1", Kind: "vm", Power: PowerRunning, HPC: hpc,
			SSHConnections: []SSHConnection{{ID: ConnectionCloudAssistant, Type: ConnectionCloudAssistant, InstallSupported: true}}}
	}
	return []Instance{
		node("i-slurm", &HPCCluster{ID: "ehpc-sz-1", Name: "slurm-lab", Scheduler: SchedulerSlurm, SchedulerVersion: "22.05.8"}),
		node("i-unread", &HPCCluster{ID: "ehpc-sz-2"}),
		node("i-pbs", &HPCCluster{ID: "ehpc-sz-3", Name: "pbs-lab", Scheduler: "pbs"}),
		node("i-plain", nil),
	}, nil
}

func (d *hpcTestDriver) PowerOn(_ context.Context, _ Credentials, id string) error {
	d.calls = append(d.calls, "on:"+id)
	return nil
}

func (d *hpcTestDriver) PowerOff(_ context.Context, _ Credentials, id string) error {
	d.calls = append(d.calls, "off:"+id)
	return nil
}

func (d *hpcTestDriver) DeleteInstance(_ context.Context, _ Credentials, id string) error {
	d.calls = append(d.calls, "delete:"+id)
	return nil
}

func (d *hpcTestDriver) ResizeInstance(_ context.Context, _ Credentials, id string, _ ResizeInstanceRequest) error {
	d.calls = append(d.calls, "resize:"+id)
	return nil
}

func newHPCService(t *testing.T, name string) (*Service, uint, *hpcTestDriver) {
	t.Helper()
	driver := &hpcTestDriver{channelTestDriver: channelTestDriver{fallbackTestDriver: fallbackTestDriver{name: name}}}
	svc, providerID := newFallbackService(t, &driver.fallbackTestDriver)
	Register(driver)
	if _, err := svc.Sync(t.Context(), providerID); err != nil {
		t.Fatal(err)
	}
	return svc, providerID, driver
}

// A sync records which HPC cluster a compute node is under, with the name
// and scheduler the vendor gave.
func TestASyncRecordsTheHPCClusterOfAComputeNode(t *testing.T) {
	svc, providerID, _ := newHPCService(t, "hpc-sync")
	row, err := svc.store.ProviderInstanceByExternalID(providerID, "i-slurm")
	if err != nil || row == nil || row.HPCClusterID != "ehpc-sz-1" || row.HPCClusterName != "slurm-lab" || row.HPCScheduler != SchedulerSlurm || row.HPCSchedulerVersion != "22.05.8" {
		t.Fatalf("Slurm node row = %+v (%v)", row, err)
	}
	if row, _ := svc.store.ProviderInstanceByExternalID(providerID, "i-plain"); row == nil || row.HPCClusterID != "" || row.HPCScheduler != "" {
		t.Fatalf("plain row = %+v", row)
	}
}

// The cluster's queue owns a compute node's lifecycle: powering, releasing
// or resizing it is refused before the vendor is called, whatever its
// scheduler; a plain instance of the account is unaffected.
func TestAnHPCComputeNodeCannotBePoweredReleasedOrResized(t *testing.T) {
	svc, providerID, driver := newHPCService(t, "hpc-power")
	ctx := context.Background()
	for _, id := range []string{"i-slurm", "i-unread", "i-pbs"} {
		for what, err := range map[string]error{
			"power on":  svc.Power(ctx, providerID, id, true),
			"power off": svc.Power(ctx, providerID, id, false),
			"release":   svc.DeleteInstance(ctx, providerID, id),
			"resize":    svc.ResizeInstance(ctx, providerID, id, ResizeInstanceRequest{GPUCount: 1}),
		} {
			var inHPC *InstanceInHPCClusterError
			if !errors.As(err, &inHPC) || !errors.Is(err, ErrInstanceInHPCCluster) || !strings.HasPrefix(inHPC.Cluster, "ehpc-sz-") {
				t.Fatalf("%s %s = %v, want InstanceInHPCClusterError", what, id, err)
			}
		}
	}
	if len(driver.calls) != 0 {
		t.Fatalf("a refused operation reached the vendor: %v", driver.calls)
	}
	if err := svc.Power(ctx, providerID, "i-plain", false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(driver.calls, ",") != "off:i-plain" {
		t.Fatalf("vendor calls = %v", driver.calls)
	}
}

// An HPC compute node joins through its cluster's entry, so installing an
// agent on it by itself is refused before it reaches the instance, whatever
// its scheduler; a plain instance installs as before.
func TestInstallOnAnHPCComputeNodeIsRefused(t *testing.T) {
	svc, providerID, driver := newHPCService(t, "hpc-install")
	ctx := t.Context()
	before := len(driver.connections)
	for _, id := range []string{"i-slurm", "i-unread", "i-pbs"} {
		_, err := svc.Install(ctx, providerID, id, "https://server.example", InstallOptions{})
		var inHPC *InstanceInHPCClusterError
		if !errors.As(err, &inHPC) || !errors.Is(err, ErrInstanceInHPCCluster) || !strings.HasPrefix(inHPC.Cluster, "ehpc-sz-") {
			t.Fatalf("install on %s = %v, want InstanceInHPCClusterError", id, err)
		}
	}
	if len(driver.connections) != before {
		t.Fatal("a refused install reached the instance")
	}
	if _, err := svc.Install(ctx, providerID, "i-plain", "https://server.example", InstallOptions{InstallDir: "/data/gpuworks/"}); err != nil || driver.last.InstallDir != "/data/gpuworks" {
		t.Fatalf("plain install dir = %q (%v)", driver.last.InstallDir, err)
	}
}
