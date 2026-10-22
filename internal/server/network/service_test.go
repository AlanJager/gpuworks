package network

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStoragePathViaService(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})

	a, err := st.RegisterNode("uuid-a", store.HashToken("a"), "a")
	if err != nil {
		t.Fatalf("register a: %+v", err)
	}
	b, err := st.RegisterNode("uuid-b", store.HashToken("b"), "b")
	if err != nil {
		t.Fatalf("register b: %+v", err)
	}
	for uuid, octet := range map[string]string{"uuid-a": "15", "uuid-b": "16"} {
		reportInventory(t, st, uuid, proto.NetInventory{
			Interfaces: []proto.NetInterface{
				{Name: "eth0", Addrs: []string{"192.168.8." + octet + "/24"}},
				{Name: "ens3f0", Addrs: []string{"10.30.1." + octet + "/16", "fd00::" + octet + "/64"}},
			},
			Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.8." + octet},
		})
	}

	// No storage network declared: the management network carries the copy.
	p, err := svc.StoragePath(a.ID, b.ID)
	if err != nil || p.Network != netplan.ManagementName || p.SourceIP != "192.168.8.15" || p.TargetIP != "192.168.8.16" {
		t.Fatalf("management path = %+v, %v", p, err)
	}

	storageNet, err := svc.CreateNetwork(NetworkSpec{
		Name: "storage", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet, CIDR: "10.30.0.0/16",
	})
	if err != nil {
		t.Fatalf("create storage network: %+v", err)
	}
	var attA *store.NetworkAttachment
	for _, uuid := range []string{"uuid-a", "uuid-b"} {
		att, err := svc.Attach(storageNet.ID, AttachmentSpec{NodeUUID: uuid})
		if err != nil {
			t.Fatalf("attach %s: %+v", uuid, err)
		}
		if uuid == "uuid-a" {
			attA = att
		}
	}
	if p, err = svc.StoragePath(a.ID, b.ID); err != nil || p.Network != "storage" || p.SourceIP != "10.30.1.15" || p.TargetIP != "10.30.1.16" {
		t.Fatalf("storage path = %+v, %v", p, err)
	}

	if _, err := svc.UpdateOverrides(storageNet.ID, attA.ID, Overrides{Interface: "ens3f0", IP: "fd00::15"}); err != nil {
		t.Fatalf("override: %+v", err)
	}
	if p, err = svc.StoragePath(a.ID, b.ID); err != nil || p.SourceIP != "fd00::15" || p.TargetIP != "10.30.1.16" {
		t.Fatalf("ipv6 override path = %+v, %v", p, err)
	}

	if _, err := svc.StoragePath(a.ID, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing node err = %+v, want ErrNotFound", err)
	}

	// A node with storage sync switched off on management and no storage
	// network of its own shares no storage path.
	c, err := st.RegisterNode("uuid-c", store.HashToken("c"), "c")
	if err != nil {
		t.Fatalf("register c: %+v", err)
	}
	d, err := st.RegisterNode("uuid-d", store.HashToken("d"), "d")
	if err != nil {
		t.Fatalf("register d: %+v", err)
	}
	if err := svc.SetManagementRolesOff("uuid-c", []string{netplan.RoleStorageSync}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	if _, err := svc.StoragePath(c.ID, d.ID); !errors.Is(err, netplan.ErrNoStoragePath) {
		t.Fatalf("no-path err = %+v, want ErrNoStoragePath", err)
	}
}

func TestServingEndpointsAndServingIP(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})

	withToken, plainToken, err := st.CreateNodePending("with-token")
	if err != nil {
		t.Fatalf("create pending: %+v", err)
	}
	if plainToken == "" {
		t.Fatal("expected a plaintext token")
	}
	reportInventory(t, st, withToken.UUID, proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.5/24"}}},
	})
	svcNet, err := svc.CreateNetwork(NetworkSpec{
		Name: "svc", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, CIDR: "10.40.0.0/24",
	})
	if err != nil {
		t.Fatalf("create serving network: %+v", err)
	}
	if _, err := svc.Attach(svcNet.ID, AttachmentSpec{NodeUUID: withToken.UUID}); err != nil {
		t.Fatalf("attach: %+v", err)
	}

	if ip := svc.ServingIP(withToken.ID); ip != "10.40.0.5" {
		t.Fatalf("serving ip = %q", ip)
	}
	eps, err := svc.ServingEndpoints()
	if err != nil {
		t.Fatalf("serving endpoints: %+v", err)
	}
	want := ServingEndpoint{IP: "10.40.0.5", NodeToken: plainToken}
	if got, ok := eps[withToken.UUID]; !ok || got != want {
		t.Fatalf("endpoint = %+v, ok=%v, want %+v", got, ok, want)
	}

	// A node whose serving network resolves to the management network (no
	// dedicated network of its own) yields no endpoint and no serving IP.
	mgmtOnly, err := st.RegisterNode("uuid-mgmt-only", store.HashToken("m"), "m")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	reportInventory(t, st, mgmtOnly.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.9.9"},
	})
	if ip := svc.ServingIP(mgmtOnly.ID); ip != "" {
		t.Fatalf("management-only serving ip = %q, want empty", ip)
	}
	eps, err = svc.ServingEndpoints()
	if err != nil {
		t.Fatalf("serving endpoints: %+v", err)
	}
	if _, ok := eps[mgmtOnly.UUID]; ok {
		t.Fatalf("management-only node got an endpoint: %+v", eps[mgmtOnly.UUID])
	}
	// The token-bearing node from above is still the only entry.
	if len(eps) != 1 {
		t.Fatalf("endpoints = %+v, want exactly 1", eps)
	}

	// A node registered with only a token hash holds a placeholder, not a
	// token its agent has: the gateway could not pin it, so it gets no
	// endpoint even on a serving network.
	noToken, err := st.RegisterNode("uuid-no-token", store.HashToken("n"), "n")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	reportInventory(t, st, noToken.UUID, proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth2", Addrs: []string{"10.40.0.9/24"}}},
	})
	if _, err := svc.Attach(svcNet.ID, AttachmentSpec{NodeUUID: noToken.UUID}); err != nil {
		t.Fatalf("attach: %+v", err)
	}
	eps, err = svc.ServingEndpoints()
	if err != nil {
		t.Fatalf("serving endpoints: %+v", err)
	}
	if ep, present := eps[noToken.UUID]; present {
		t.Fatalf("placeholder-token node got an endpoint: %+v", ep)
	}
	if ip := svc.ServingIP(noToken.ID); ip != "10.40.0.9" {
		t.Fatalf("its forwarder address = %q", ip)
	}
}

func TestCanServe(t *testing.T) {
	// A node whose management network may serve is answered true without the
	// catalog ever being read.
	failing := New(newStore(t), failCatalog{err: errors.New("must not be read")})
	if !failing.CanServe(store.Node{ID: 1}) {
		t.Fatal("CanServe on a node with serving allowed = false")
	}

	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n1, err := st.RegisterNode("uuid-1", store.HashToken("1"), "n1")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	if err := svc.SetManagementRolesOff(n1.UUID, []string{netplan.RoleServing}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	reloaded, err := st.NodeByUUID(n1.UUID)
	if err != nil {
		t.Fatalf("reload: %+v", err)
	}
	if svc.CanServe(*reloaded) {
		t.Fatal("CanServe true with role off and no serving network")
	}

	n2, err := st.RegisterNode("uuid-2", store.HashToken("2"), "n2")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	if err := svc.SetManagementRolesOff(n2.UUID, []string{netplan.RoleServing}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	svcNet, err := svc.CreateNetwork(NetworkSpec{Name: "own-serving", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	if _, err := svc.Attach(svcNet.ID, AttachmentSpec{NodeUUID: n2.UUID}); err != nil {
		t.Fatalf("attach: %+v", err)
	}
	reloaded2, err := st.NodeByUUID(n2.UUID)
	if err != nil {
		t.Fatalf("reload: %+v", err)
	}
	if !svc.CanServe(*reloaded2) {
		t.Fatal("CanServe false with role off but an own serving network")
	}

	// Role off and a catalog that cannot be read refuses.
	failingWithRoleOff := New(st, failCatalog{err: errors.New("boom")})
	if failingWithRoleOff.CanServe(*reloaded) {
		t.Fatal("CanServe true with role off and a failing catalog")
	}
}

func TestComputeGroupSharedRDMANetwork(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	p, err := st.RegisterNode("uuid-p", store.HashToken("p"), "p")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	q, err := st.RegisterNode("uuid-q", store.HashToken("q"), "q")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	for _, uuid := range []string{p.UUID, q.UUID} {
		reportInventory(t, st, uuid, proto.NetInventory{
			RDMAPorts: []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand"}},
		})
	}
	rdmaNet, err := svc.CreateNetwork(NetworkSpec{Name: "ib-a", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostRDMA})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	for _, uuid := range []string{p.UUID, q.UUID} {
		if _, err := svc.Attach(rdmaNet.ID, AttachmentSpec{NodeUUID: uuid}); err != nil {
			t.Fatalf("attach %s: %+v", uuid, err)
		}
	}
	pReloaded, _ := st.NodeByUUID("uuid-p")
	qReloaded, _ := st.NodeByUUID("uuid-q")
	group, err := svc.ComputeGroup([]store.Node{*pReloaded, *qReloaded})
	if err != nil {
		t.Fatalf("compute group: %+v", err)
	}
	if group.Network.ID != rdmaNet.ID {
		t.Fatalf("group network = %+v, want %d", group.Network, rdmaNet.ID)
	}
	if len(group.Ranks) != 2 {
		t.Fatalf("ranks = %+v", group.Ranks)
	}
}

func TestComputeGroupFallsBackToManagement(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	m1, err := st.RegisterNode("uuid-m1", store.HashToken("m1"), "m1")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	m2, err := st.RegisterNode("uuid-m2", store.HashToken("m2"), "m2")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	reportInventory(t, st, m1.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.1.1"},
	})
	reportInventory(t, st, m2.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.1.2"},
	})
	n1, _ := st.NodeByUUID("uuid-m1")
	n2, _ := st.NodeByUUID("uuid-m2")
	group, err := svc.ComputeGroup([]store.Node{*n1, *n2})
	if err != nil {
		t.Fatalf("compute group: %+v", err)
	}
	if group.Network.Name != netplan.ManagementName {
		t.Fatalf("group network = %+v, want management", group.Network)
	}
}

func TestComputeGroupErrorWhenRoleOffAndNoNetwork(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	r, err := st.RegisterNode("uuid-r", store.HashToken("r"), "r")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	s2, err := st.RegisterNode("uuid-s", store.HashToken("s"), "s")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	if err := svc.SetManagementRolesOff(r.UUID, []string{netplan.RoleCompute}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	reportInventory(t, st, s2.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.1.9"},
	})
	rReloaded, _ := st.NodeByUUID(r.UUID)
	sReloaded, _ := st.NodeByUUID(s2.UUID)
	_, err = svc.ComputeGroup([]store.Node{*rReloaded, *sReloaded})
	var groupErr *netplan.GroupError
	if !errors.As(err, &groupErr) {
		t.Fatalf("err = %+v, want *netplan.GroupError", err)
	}
	if groupErr.Reason != netplan.ComputeNone || groupErr.Node.UUID != r.UUID {
		t.Fatalf("group error = %+v", groupErr)
	}
}

func TestEndpoints(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	withMgmt, err := st.RegisterNode("uuid-with-mgmt", store.HashToken("w"), "w")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	withoutMgmt, err := st.RegisterNode("uuid-without-mgmt", store.HashToken("x"), "x")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	reportInventory(t, st, withMgmt.UUID, proto.NetInventory{
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.2.5"},
	})
	withMgmtReloaded, _ := st.NodeByUUID(withMgmt.UUID)
	withoutMgmtReloaded, _ := st.NodeByUUID(withoutMgmt.UUID)
	nodes := []store.Node{*withMgmtReloaded, *withoutMgmtReloaded}

	mgmtEndpoints, err := svc.Endpoints(0, nodes)
	if err != nil {
		t.Fatalf("management endpoints: %+v", err)
	}
	if len(mgmtEndpoints) != 1 || mgmtEndpoints[0].NodeID != withMgmt.ID || mgmtEndpoints[0].IP != "192.168.2.5" {
		t.Fatalf("management endpoints = %+v", mgmtEndpoints)
	}

	computeNet, err := svc.CreateNetwork(NetworkSpec{Name: "compute", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	if _, err := svc.Attach(computeNet.ID, AttachmentSpec{NodeUUID: withMgmt.UUID}); err != nil {
		t.Fatalf("attach: %+v", err)
	}
	withMgmtReloaded, _ = st.NodeByUUID(withMgmt.UUID)
	nodes = []store.Node{*withMgmtReloaded, *withoutMgmtReloaded}
	netEndpoints, err := svc.Endpoints(computeNet.ID, nodes)
	if err != nil {
		t.Fatalf("network endpoints: %+v", err)
	}
	if len(netEndpoints) != 1 || netEndpoints[0].NodeID != withMgmt.ID || netEndpoints[0].Network.ID != computeNet.ID {
		t.Fatalf("network endpoints = %+v", netEndpoints)
	}
}

func TestStateResolveNodeNetworksHappyPath(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := st.RegisterNode("uuid-happy", store.HashToken("h"), "h")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	net, err := svc.CreateNetwork(NetworkSpec{Name: "happy", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	if _, err := svc.Attach(net.ID, AttachmentSpec{NodeUUID: n.UUID}); err != nil {
		t.Fatalf("attach: %+v", err)
	}

	state, err := svc.State()
	if err != nil {
		t.Fatalf("state: %+v", err)
	}
	if len(state.Nodes) != 1 || len(state.Networks) != 1 || len(state.Attachments) != 1 {
		t.Fatalf("state = %+v", state)
	}
	if _, ok := state.Resolved.For(n.ID, netplan.RoleCompute); !ok {
		t.Fatalf("state resolved missing compute role: %+v", state.Resolved)
	}

	resolved, err := svc.Resolve(state.Nodes)
	if err != nil {
		t.Fatalf("resolve: %+v", err)
	}
	if r, ok := resolved.For(n.ID, netplan.RoleCompute); !ok || r.Network.ID != net.ID {
		t.Fatalf("resolve = %+v", resolved)
	}

	nodeNetworks, err := svc.NodeNetworks(n.ID)
	if err != nil {
		t.Fatalf("node networks: %+v", err)
	}
	if r, ok := nodeNetworks.For(n.ID, netplan.RoleCompute); !ok || r.Network.ID != net.ID {
		t.Fatalf("node networks = %+v", nodeNetworks)
	}
}
