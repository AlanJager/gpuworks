package network

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestManagementOnlyCatalogIgnoresDeclaredNetworks(t *testing.T) {
	st := newStore(t)
	// A dedicated network row exists in the store, but ManagementOnly must
	// never surface it.
	setup := New(st, StoreCatalog{Store: st})
	dedicated, err := setup.CreateNetwork(NetworkSpec{
		Name: "dedicated", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, CIDR: "10.50.0.0/24",
	})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	n, err := st.RegisterNode("uuid-mo", store.HashToken("mo"), "mo")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	if _, err := setup.Attach(dedicated.ID, AttachmentSpec{NodeUUID: n.UUID}); err != nil {
		t.Fatalf("attach: %+v", err)
	}
	reportInventory(t, st, n.UUID, proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{"10.50.0.9/24"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.7.9"},
	})

	svc := New(st, ManagementOnly{})
	if svc.Dedicated() {
		t.Fatal("ManagementOnly reports Dedicated() true")
	}

	state, err := svc.State()
	if err != nil {
		t.Fatalf("state: %+v", err)
	}
	if len(state.Networks) != 0 || len(state.Attachments) != 0 {
		t.Fatalf("state carries declared networks: %+v", state)
	}
	if r, ok := state.Resolved.For(n.ID, netplan.RoleServing); !ok || !r.IsManagement() {
		t.Fatalf("resolved serving role = %+v, ok=%v, want management", r, ok)
	}

	nReloaded, _ := st.NodeByUUID(n.UUID)
	group, err := svc.ComputeGroup([]store.Node{*nReloaded})
	if err != nil {
		t.Fatalf("compute group: %+v", err)
	}
	if group.Network.Name != netplan.ManagementName {
		t.Fatalf("group network = %+v", group.Network)
	}

	second, err := st.RegisterNode("uuid-mo2", store.HashToken("mo2"), "mo2")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	reportInventory(t, st, second.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.7.10"},
	})
	path, err := svc.StoragePath(n.ID, second.ID)
	if err != nil || path.Network != netplan.ManagementName {
		t.Fatalf("storage path = %+v, %v", path, err)
	}

	eps, err := svc.ServingEndpoints()
	if err != nil {
		t.Fatalf("serving endpoints: %+v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("management-only serving endpoints = %+v, want none", eps)
	}

	// Every write to declared networks is rejected.
	if _, err := svc.CreateNetwork(NetworkSpec{Name: "x", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("create err = %+v", err)
	}
	if _, err := svc.UpdateNetwork(dedicated.ID, NetworkSpec{Name: "x", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("update err = %+v", err)
	}
	if err := svc.DeleteNetwork(dedicated.ID); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("delete err = %+v", err)
	}
	if _, err := svc.Attach(dedicated.ID, AttachmentSpec{NodeUUID: second.UUID}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("attach err = %+v", err)
	}
	if _, err := svc.UpdateOverrides(dedicated.ID, 1, Overrides{}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("update overrides err = %+v", err)
	}
	if err := svc.Detach(dedicated.ID, 1); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("detach err = %+v", err)
	}

	// Management roles still work: they are node-side state, not catalog rows.
	if err := svc.SetManagementRolesOff(n.UUID, []string{netplan.RoleServing}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	if changed, err := svc.UpdateManagementRoles([]string{n.UUID}, nil, []string{netplan.RoleCompute}); err != nil || changed != 1 {
		t.Fatalf("update roles = %d, %+v", changed, err)
	}
}

func TestFailingCatalogPropagates(t *testing.T) {
	st := newStore(t)
	sentinel := errors.New("catalog unavailable")
	svc := New(st, failCatalog{err: sentinel, dedicated: true})

	n, err := st.RegisterNode("uuid-fail", store.HashToken("f"), "f")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	reportInventory(t, st, n.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.3.1"},
	})
	nReloaded, _ := st.NodeByUUID(n.UUID)
	nodes := []store.Node{*nReloaded}

	if _, err := svc.State(); !errors.Is(err, sentinel) {
		t.Fatalf("State err = %+v", err)
	}
	if _, err := svc.Resolve(nodes); !errors.Is(err, sentinel) {
		t.Fatalf("Resolve err = %+v", err)
	}
	if _, err := svc.NodeNetworks(n.ID); !errors.Is(err, sentinel) {
		t.Fatalf("NodeNetworks err = %+v", err)
	}
	if _, err := svc.ComputeGroup(nodes); !errors.Is(err, sentinel) {
		t.Fatalf("ComputeGroup err = %+v", err)
	}
	if _, err := svc.StoragePath(n.ID, n.ID); !errors.Is(err, sentinel) {
		t.Fatalf("StoragePath err = %+v", err)
	}
	if _, err := svc.ServingEndpoints(); !errors.Is(err, sentinel) {
		t.Fatalf("ServingEndpoints err = %+v", err)
	}
	if _, err := svc.Endpoints(1, nodes); !errors.Is(err, sentinel) {
		t.Fatalf("Endpoints(1) err = %+v", err)
	}
	// Endpoints(0) never reads the catalog.
	mgmtEndpoints, err := svc.Endpoints(0, nodes)
	if err != nil {
		t.Fatalf("Endpoints(0) err = %+v", err)
	}
	if len(mgmtEndpoints) != 1 || mgmtEndpoints[0].NodeID != n.ID {
		t.Fatalf("Endpoints(0) = %+v", mgmtEndpoints)
	}
	if ip := svc.ServingIP(n.ID); ip != "" {
		t.Fatalf("ServingIP = %q, want empty on a failing catalog", ip)
	}
}

func TestServiceDedicated(t *testing.T) {
	st := newStore(t)
	if !(New(st, StoreCatalog{Store: st})).Dedicated() {
		t.Fatal("StoreCatalog service reports Dedicated() false")
	}
	if (New(st, ManagementOnly{})).Dedicated() {
		t.Fatal("ManagementOnly service reports Dedicated() true")
	}
}
