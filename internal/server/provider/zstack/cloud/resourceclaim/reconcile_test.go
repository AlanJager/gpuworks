package resourceclaim

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

type leaseInspectorTestDriver struct {
	state string
}

func (d *leaseInspectorTestDriver) Name() string { return "lease-inspector-test-provider" }

func (d *leaseInspectorTestDriver) Capabilities() provider.Capabilities {
	return provider.Capabilities{}
}

func (d *leaseInspectorTestDriver) ValidateCredentials(context.Context, provider.Credentials) error {
	return nil
}

func (d *leaseInspectorTestDriver) ListInstances(context.Context, provider.Credentials) ([]provider.Instance, error) {
	return nil, nil
}

func (d *leaseInspectorTestDriver) PowerState(context.Context, provider.Credentials, string) (provider.PowerState, error) {
	return provider.PowerUnknown, nil
}

func (d *leaseInspectorTestDriver) PowerOn(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *leaseInspectorTestDriver) PowerOff(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *leaseInspectorTestDriver) CreateInstance(context.Context, provider.Credentials, provider.CreateInstanceRequest) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrUnsupported
}

func (d *leaseInspectorTestDriver) DeleteInstance(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *leaseInspectorTestDriver) ResizeInstance(context.Context, provider.Credentials, string, provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

func (d *leaseInspectorTestDriver) InstallAgent(context.Context, provider.Credentials, provider.InstallRequest) error {
	return provider.ErrUnsupported
}

func (d *leaseInspectorTestDriver) InspectResource(_ context.Context, _ provider.Credentials, mutation ResourceMutation) (ResourceObservation, error) {
	return ResourceObservation{ResourceType: mutation.ResourceType, ResourceID: mutation.ResourceID, State: d.state}, nil
}

func TestReconcileCloudResourceLeaseConfirmsInterruptedGPUWrite(t *testing.T) {
	driver := &leaseInspectorTestDriver{state: "Disabled"}
	provider.Register(driver)
	st, err := store.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("cloud-lease-reconcile-test")
	if err != nil {
		t.Fatal(err)
	}
	creds, _ := json.Marshal(provider.Credentials{Endpoint: "http://cloud.example", PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	cipher, err := box.Encrypt(creds)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProvider(&store.Provider{Name: "cloud", Kind: driver.Name(), CredsCipher: cipher})
	if err != nil {
		t.Fatal(err)
	}
	host := cloudstore.CloudHost{ProviderID: p.ID, ExternalID: "host-1"}
	if err := cloudstore.New(st.DB()).SaveCloudHostSnapshot(host); err != nil {
		t.Fatal(err)
	}
	hosts, err := cloudstore.New(st.DB()).CloudHosts(p.ID)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts = %+v err=%v", hosts, err)
	}
	lease, err := cloudstore.New(st.DB()).CreateCloudResourceLease(&cloudstore.CloudResourceLease{
		OperationID: "gpu-unknown", ProviderID: p.ID, CloudHostID: hosts[0].ID,
		ResourceType: "gpu", ResourceID: "gpu-1", Amount: 1, Unit: "device", State: cloudstore.CloudLeaseReservePending,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := New(st, provider.NewService(st, box))
	if err := service.ReconcileCloudResourceLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated, err := cloudstore.New(st.DB()).CloudResourceLeaseByID(lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != cloudstore.CloudLeaseReserved || updated.ErrorCode != "confirmed_reserved" {
		t.Fatalf("lease after reconciliation = %+v", updated)
	}
}
