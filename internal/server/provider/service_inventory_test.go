package provider

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// inventoryTestDriver models a vendor account: instances exist until a
// successful release, releasing an already-gone instance succeeds (as the real
// drivers treat 404), and listing or releasing can be made to fail.
type inventoryTestDriver struct {
	mu        sync.Mutex
	name      string
	instances map[string]Instance
	listErr   error
	deleteErr error
	deleted   []string
}

func newInventoryTestDriver(name string, instances ...Instance) *inventoryTestDriver {
	d := &inventoryTestDriver{name: name, instances: map[string]Instance{}}
	for _, inst := range instances {
		d.instances[inst.ExternalID] = inst
	}
	return d
}

func (d *inventoryTestDriver) release(externalID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.instances, externalID)
}

func (d *inventoryTestDriver) Name() string { return d.name }

func (d *inventoryTestDriver) Capabilities() Capabilities {
	return Capabilities{CanListInstances: true}
}

func (d *inventoryTestDriver) ValidateCredentials(context.Context, Credentials) error { return nil }

func (d *inventoryTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	out := make([]Instance, 0, len(d.instances))
	for _, inst := range d.instances {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalID < out[j].ExternalID })
	return out, nil
}

func (d *inventoryTestDriver) PowerState(context.Context, Credentials, string) (PowerState, error) {
	return PowerUnknown, ErrUnsupported
}

func (d *inventoryTestDriver) PowerOn(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (d *inventoryTestDriver) PowerOff(context.Context, Credentials, string) error {
	return ErrUnsupported
}

func (d *inventoryTestDriver) CreateInstance(context.Context, Credentials, CreateInstanceRequest) (Instance, error) {
	return Instance{}, ErrUnsupported
}

func (d *inventoryTestDriver) DeleteInstance(_ context.Context, _ Credentials, externalID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted = append(d.deleted, externalID)
	if d.deleteErr != nil {
		return d.deleteErr
	}
	delete(d.instances, externalID)
	return nil
}

func (d *inventoryTestDriver) ResizeInstance(context.Context, Credentials, string, ResizeInstanceRequest) error {
	return ErrUnsupported
}

func (d *inventoryTestDriver) InstallAgent(context.Context, Credentials, InstallRequest) error {
	return ErrUnsupported
}

type inventoryFixture struct {
	svc  *Service
	st   *store.Store
	drv  *inventoryTestDriver
	prov *store.Provider
}

func newInventoryFixture(t *testing.T, instances ...Instance) inventoryFixture {
	t.Helper()
	drv := newInventoryTestDriver("inventory-test-"+t.Name(), instances...)
	Register(drv)
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("provider-inventory-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	p, err := svc.Add(context.Background(), drv.name, "cloud", Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return inventoryFixture{svc: svc, st: st, drv: drv, prov: p}
}

// rows returns the provider's stored rows keyed by external id.
func (f inventoryFixture) rows(t *testing.T) map[string]store.ProviderInstance {
	t.Helper()
	pis, err := f.st.ProviderInstances(f.prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]store.ProviderInstance, len(pis))
	for _, pi := range pis {
		out[pi.ExternalID] = pi
	}
	return out
}

func (f inventoryFixture) linkNode(t *testing.T, externalID, name string) *store.Node {
	t.Helper()
	node, _, err := f.st.CreateNodePending(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.LinkProviderInstanceNode(f.prov.ID, externalID, node.ID); err != nil {
		t.Fatal(err)
	}
	return node
}

func externalIDs(rows map[string]store.ProviderInstance) []string {
	out := make([]string, 0, len(rows))
	for id := range rows {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func TestServiceDeleteInstanceRemovesRowAfterVendorRelease(t *testing.T) {
	f := newInventoryFixture(t,
		Instance{ExternalID: "uhost-a", Name: "a", Power: PowerRunning},
		Instance{ExternalID: "uhost-b", Name: "b", Power: PowerRunning},
	)
	if _, err := f.svc.Sync(context.Background(), f.prov.ID); err != nil {
		t.Fatal(err)
	}
	node := f.linkNode(t, "uhost-a", "a")

	if err := f.svc.DeleteInstance(context.Background(), f.prov.ID, "uhost-a"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if !reflect.DeepEqual(f.drv.deleted, []string{"uhost-a"}) {
		t.Fatalf("vendor delete calls = %v", f.drv.deleted)
	}
	if got := externalIDs(f.rows(t)); !reflect.DeepEqual(got, []string{"uhost-b"}) {
		t.Fatalf("rows after delete = %v", got)
	}
	linked, err := f.st.NodeByUUID(node.UUID)
	if err != nil {
		t.Fatalf("the node installed on a released instance must be kept: %v", err)
	}
	if linked.RecoveryPhase != store.RecoveryPhaseStopped || linked.LastRecoveryError != store.RecoveryStoppedProviderInstanceGone {
		t.Fatalf("node recovery = %q/%q", linked.RecoveryPhase, linked.LastRecoveryError)
	}

	// A follow-up sync agrees with the deletion and does not bring the row back.
	if _, err := f.svc.Sync(context.Background(), f.prov.ID); err != nil {
		t.Fatal(err)
	}
	if got := externalIDs(f.rows(t)); !reflect.DeepEqual(got, []string{"uhost-b"}) {
		t.Fatalf("rows after sync = %v", got)
	}
}

func TestServiceDeleteInstanceKeepsRowWhenVendorFails(t *testing.T) {
	f := newInventoryFixture(t, Instance{ExternalID: "uhost-a", Name: "a", Power: PowerRunning})
	if _, err := f.svc.Sync(context.Background(), f.prov.ID); err != nil {
		t.Fatal(err)
	}
	node := f.linkNode(t, "uhost-a", "a")
	vendorErr := errors.New("compshare: API RetCode 8039: instance is locked")
	f.drv.deleteErr = vendorErr

	err := f.svc.DeleteInstance(context.Background(), f.prov.ID, "uhost-a")
	if !errors.Is(err, vendorErr) {
		t.Fatalf("DeleteInstance err = %v, want vendor error", err)
	}
	row, ok := f.rows(t)["uhost-a"]
	if !ok || row.Power != string(PowerRunning) || row.NodeID != node.ID {
		t.Fatalf("row after failed delete = %+v (present=%v)", row, ok)
	}
	if linked, err := f.st.NodeByUUID(node.UUID); err != nil || linked.RecoveryPhase != "" {
		t.Fatalf("node after failed delete = %+v, err=%v", linked, err)
	}
}

func TestServiceSyncRemovesInstancesTheVendorNoLongerLists(t *testing.T) {
	f := newInventoryFixture(t,
		Instance{ExternalID: "uhost-a", Name: "a", Power: PowerRunning},
		Instance{ExternalID: "uhost-b", Name: "b", Power: PowerRunning},
		Instance{ExternalID: "uhost-c", Name: "c", Power: PowerRunning},
	)
	if _, err := f.svc.Sync(context.Background(), f.prov.ID); err != nil {
		t.Fatal(err)
	}
	nodeA := f.linkNode(t, "uhost-a", "a")
	nodeC := f.linkNode(t, "uhost-c", "c")

	// Released outside gpuworks (console, expiry); a is stopped meanwhile.
	f.drv.release("uhost-c")
	f.drv.mu.Lock()
	f.drv.instances["uhost-a"] = Instance{ExternalID: "uhost-a", Name: "a", Power: PowerStopped}
	f.drv.mu.Unlock()

	listed, err := f.svc.Sync(context.Background(), f.prov.ID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("Sync returned %d instances, want 2", len(listed))
	}
	rows := f.rows(t)
	if got := externalIDs(rows); !reflect.DeepEqual(got, []string{"uhost-a", "uhost-b"}) {
		t.Fatalf("rows after sync = %v", got)
	}
	if rows["uhost-a"].NodeID != nodeA.ID || rows["uhost-a"].Power != string(PowerStopped) {
		t.Fatalf("surviving row = %+v, want node %d and stopped", rows["uhost-a"], nodeA.ID)
	}
	if n, err := f.st.NodeByUUID(nodeA.UUID); err != nil || n.RecoveryPhase != "" {
		t.Fatalf("node of a listed instance = %+v, err=%v", n, err)
	}
	if n, err := f.st.NodeByUUID(nodeC.UUID); err != nil || n.RecoveryPhase != store.RecoveryPhaseStopped {
		t.Fatalf("node of a released instance = %+v, err=%v", n, err)
	}

	// Every instance released: a successful empty listing clears the table.
	f.drv.release("uhost-a")
	f.drv.release("uhost-b")
	listed, err = f.svc.Sync(context.Background(), f.prov.ID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("Sync(empty) = %v, %v", listed, err)
	}
	if got := f.rows(t); len(got) != 0 {
		t.Fatalf("rows after empty listing = %v", externalIDs(got))
	}
}

func TestServiceSyncListFailureLeavesRowsUntouched(t *testing.T) {
	f := newInventoryFixture(t,
		Instance{ExternalID: "uhost-a", Name: "a", Power: PowerRunning},
		Instance{ExternalID: "uhost-b", Name: "b", Power: PowerRunning},
	)
	if _, err := f.svc.Sync(context.Background(), f.prov.ID); err != nil {
		t.Fatal(err)
	}
	node := f.linkNode(t, "uhost-a", "a")
	f.drv.release("uhost-b")
	listErr := errors.New("compshare: HTTP 502: bad gateway")
	f.drv.listErr = listErr

	if _, err := f.svc.Sync(context.Background(), f.prov.ID); !errors.Is(err, listErr) {
		t.Fatalf("Sync err = %v, want list error", err)
	}
	rows := f.rows(t)
	if got := externalIDs(rows); !reflect.DeepEqual(got, []string{"uhost-a", "uhost-b"}) {
		t.Fatalf("rows after failed listing = %v", got)
	}
	if rows["uhost-a"].NodeID != node.ID {
		t.Fatalf("node link lost: %+v", rows["uhost-a"])
	}
	p, err := f.st.GetProvider(f.prov.ID)
	if err != nil || p.State != "invalid" {
		t.Fatalf("provider state = %+v, err=%v", p, err)
	}
	if n, err := f.st.NodeByUUID(node.UUID); err != nil || n.RecoveryPhase != "" {
		t.Fatalf("node after failed listing = %+v, err=%v", n, err)
	}
}
