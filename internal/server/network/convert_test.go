package network

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNodeDecodesLabelsInventoryAndRolesOff(t *testing.T) {
	n := store.Node{
		ID:                 7,
		UUID:               "uuid-1",
		Name:               "gpu-a",
		Labels:             `{"pool":"a"}`,
		ManagementRolesOff: `["serving"]`,
		NetworkInventory:   `{"interfaces":[{"name":"eth0","addrs":["10.0.0.5/24"]}],"management":{"interface":"eth0","ip":"10.0.0.5"}}`,
	}
	got := Node(n)
	if got.ID != 7 || got.UUID != "uuid-1" || got.Name != "gpu-a" {
		t.Fatalf("node = %+v", got)
	}
	if !reflect.DeepEqual(got.Labels, map[string]string{"pool": "a"}) {
		t.Fatalf("labels = %#v", got.Labels)
	}
	if !reflect.DeepEqual(got.RolesOff, map[string]bool{netplan.RoleServing: true}) {
		t.Fatalf("rolesOff = %#v", got.RolesOff)
	}
	if got.Inventory == nil || len(got.Inventory.Interfaces) != 1 || got.Inventory.Interfaces[0].Name != "eth0" {
		t.Fatalf("inventory = %+v", got.Inventory)
	}
	addr, ok := got.Management()
	if !ok || addr.IP != "10.0.0.5" || addr.Interface != "eth0" {
		t.Fatalf("management = %+v, %v", addr, ok)
	}
}

func TestNodeNameFallsBackToHostnameThenUUID(t *testing.T) {
	if got := Node(store.Node{UUID: "abcdefghijk"}).Name; got != "abcdefgh" {
		t.Fatalf("name from uuid = %q", got)
	}
	if got := Node(store.Node{UUID: "abcdefghijk", Hostname: "host-1"}).Name; got != "host-1" {
		t.Fatalf("name from hostname = %q", got)
	}
	if got := Node(store.Node{UUID: "abcdefghijk", Hostname: "host-1", Name: "operator-name"}).Name; got != "operator-name" {
		t.Fatalf("name from Name = %q", got)
	}
}

func TestNodeCorruptLabelsAndInventoryDecodeEmpty(t *testing.T) {
	n := store.Node{UUID: "u", Labels: "{not json", NetworkInventory: "{not json"}
	got := Node(n)
	if got.Labels == nil || len(got.Labels) != 0 {
		t.Fatalf("labels = %#v, want empty map", got.Labels)
	}
	if got.Inventory != nil {
		t.Fatalf("inventory = %+v, want nil", got.Inventory)
	}
	if _, ok := got.Management(); ok {
		t.Fatal("management ok with nil inventory")
	}
}

func TestInventoryBlankIsNil(t *testing.T) {
	if got := Inventory(store.Node{UUID: "u", NetworkInventory: "   "}); got != nil {
		t.Fatalf("inventory = %+v, want nil", got)
	}
	if got := Inventory(store.Node{UUID: "u", NetworkInventory: "not json"}); got != nil {
		t.Fatalf("inventory = %+v, want nil", got)
	}
}

func TestNodesConvertsInOrder(t *testing.T) {
	out := Nodes([]store.Node{{ID: 1}, {ID: 2}, {ID: 3}})
	if len(out) != 3 || out[0].ID != 1 || out[1].ID != 2 || out[2].ID != 3 {
		t.Fatalf("nodes = %+v", out)
	}
}

func TestRolesOffDecodesStoredList(t *testing.T) {
	got := RolesOff(store.Node{ManagementRolesOff: `["storage_sync","compute"]`})
	want := []string{"storage_sync", "compute"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rolesOff = %v, want %v", got, want)
	}
	if got := RolesOff(store.Node{}); len(got) != 0 {
		t.Fatalf("rolesOff of blank = %v", got)
	}
}

func TestNetworkConvertsFields(t *testing.T) {
	sn := store.Network{
		ID: 3, Name: "ib-a", Roles: `["compute"]`, Type: "host_rdma",
		CIDR: "10.0.0.0/24", Interface: "eth*",
	}
	got := Network(sn)
	want := netplan.Network{
		ID: 3, Name: "ib-a", Roles: []string{"compute"}, Type: "host_rdma",
		CIDR: "10.0.0.0/24", Interface: "eth*",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("network = %+v, want %+v", got, want)
	}
}

func TestAttachmentConvertsFields(t *testing.T) {
	sa := store.NetworkAttachment{
		ID: 5, NetworkID: 3, NodeID: 9, Interface: "bond0", IP: "10.0.0.2",
		HCAs: `["mlx5_0","mlx5_1"]`,
	}
	got := Attachment(sa)
	want := netplan.Attachment{
		ID: 5, NetworkID: 3, NodeID: 9, Interface: "bond0", IP: "10.0.0.2",
		HCAs: []string{"mlx5_0", "mlx5_1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachment = %+v, want %+v", got, want)
	}
	// A label attachment carries no node id or overrides.
	sa2 := store.NetworkAttachment{ID: 6, NetworkID: 3, LabelKey: "pool", LabelValue: "a"}
	got2 := Attachment(sa2)
	if got2.NodeID != 0 || got2.LabelKey != "pool" || got2.LabelValue != "a" || len(got2.HCAs) != 0 {
		t.Fatalf("label attachment = %+v", got2)
	}
}

func TestManagementAddressFalseBeforeReport(t *testing.T) {
	if _, ok := ManagementAddress(store.Node{UUID: "u"}); ok {
		t.Fatal("management address true before any report")
	}
	n := store.Node{UUID: "u", NetworkInventory: `{"management":{"interface":"eth0","ip":"10.0.0.9"}}`}
	addr, ok := ManagementAddress(n)
	if !ok || addr.IP != "10.0.0.9" {
		t.Fatalf("management = %+v, %v", addr, ok)
	}
}
