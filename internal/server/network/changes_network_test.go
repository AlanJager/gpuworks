package network

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCreateNetworkValidation(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})

	longName := ""
	for len(longName) < 65 {
		longName += "a"
	}

	cases := []struct {
		name               string
		spec               NetworkSpec
		kind, field, value string
	}{
		{"empty name", NetworkSpec{Name: "", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet}, InvalidName, "", ""},
		{"65-char name", NetworkSpec{Name: longName, Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet}, InvalidName, "", ""},
		{"unknown role", NetworkSpec{Name: "n", Roles: []string{" bogus "}, Type: netplan.TypeHostEthernet}, InvalidRole, "", "bogus"},
		{"no roles", NetworkSpec{Name: "n", Roles: nil, Type: netplan.TypeHostEthernet}, InvalidRole, "", ""},
		{"bad type", NetworkSpec{Name: "n", Roles: []string{netplan.RoleServing}, Type: "bogus-type"}, InvalidType, "", "bogus-type"},
		{"bad cidr", NetworkSpec{Name: "n", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, CIDR: "not-a-cidr"}, InvalidSelector, "cidr", "not-a-cidr"},
		{"interface glob with space", NetworkSpec{Name: "n", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, Interface: "bad iface"}, InvalidSelector, "interface", "bad iface"},
		{"interface glob with slash", NetworkSpec{Name: "n", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, Interface: "a/b"}, InvalidSelector, "interface", "a/b"},
		// An RDMA network infers its bootstrap interface; only a CIDR may narrow it.
		{"interface glob on rdma", NetworkSpec{Name: "n", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostRDMA, Interface: "ib*"}, InvalidSelector, "interface", "ib*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.CreateNetwork(c.spec)
			assertInvalid(t, err, c.kind, c.field, c.value)
		})
	}
}

func TestCreateNetworkNormalizesCIDR(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{Name: "cidr-net", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, CIDR: "10.20.1.5/16"})
	if err != nil {
		t.Fatalf("create: %+v", err)
	}
	if n.CIDR != "10.20.0.0/16" {
		t.Fatalf("cidr = %q, want 10.20.0.0/16", n.CIDR)
	}
}

func TestCreateNetworkDeduplicatesRolesInDisplayOrder(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{
		Name: "roles-net", Type: netplan.TypeHostEthernet,
		Roles: []string{netplan.RoleServing, netplan.RoleStorageSync, netplan.RoleServing, netplan.RoleCompute},
	})
	if err != nil {
		t.Fatalf("create: %+v", err)
	}
	got := store.DecodeStringList(n.Roles)
	want := []string{netplan.RoleStorageSync, netplan.RoleCompute, netplan.RoleServing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

func TestUpdateNetworkValidatesAndDedicationGate(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{Name: "u", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create: %+v", err)
	}
	_, err = svc.UpdateNetwork(n.ID, NetworkSpec{Name: "", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet})
	assertInvalid(t, err, InvalidName, "", "")

	updated, err := svc.UpdateNetwork(n.ID, NetworkSpec{Name: "u2", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostEthernet})
	if err != nil || updated.Name != "u2" {
		t.Fatalf("update = %+v, %v", updated, err)
	}

	managementOnly := New(st, ManagementOnly{})
	if _, err := managementOnly.UpdateNetwork(n.ID, NetworkSpec{Name: "x", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("update err = %+v, want ErrDedicatedUnavailable", err)
	}
}

func TestDeleteNetwork(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{Name: "d", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create: %+v", err)
	}
	if err := svc.DeleteNetwork(n.ID); err != nil {
		t.Fatalf("delete: %+v", err)
	}
	if err := svc.DeleteNetwork(n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete err = %+v, want ErrNotFound", err)
	}

	managementOnly := New(st, ManagementOnly{})
	if err := managementOnly.DeleteNetwork(1); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("delete err = %+v, want ErrDedicatedUnavailable", err)
	}
}

func TestAttachToNode(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "eth", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	rdma, err := svc.CreateNetwork(NetworkSpec{Name: "rdma", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostRDMA})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	node, err := st.RegisterNode("uuid-attach", store.HashToken("a"), "a")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}

	att, err := svc.Attach(eth.ID, AttachmentSpec{NodeUUID: node.UUID, Overrides: Overrides{Interface: "bond0", IP: " 10.0.0.1 "}})
	if err != nil {
		t.Fatalf("attach: %+v", err)
	}
	if att.Interface != "bond0" || att.IP != "10.0.0.1" || att.NodeID != node.ID {
		t.Fatalf("attachment = %+v", att)
	}

	_, err = svc.Attach(eth.ID, AttachmentSpec{NodeUUID: node.UUID, Overrides: Overrides{Interface: "bad iface"}})
	assertInvalid(t, err, InvalidOverride, "interface", "bad iface")

	_, err = svc.Attach(eth.ID, AttachmentSpec{NodeUUID: node.UUID, Overrides: Overrides{IP: "not-an-ip"}})
	assertInvalid(t, err, InvalidOverride, "ip", "not-an-ip")

	_, err = svc.Attach(eth.ID, AttachmentSpec{NodeUUID: node.UUID, Overrides: Overrides{HCAs: []string{"mlx5_0"}}})
	assertInvalid(t, err, InvalidOverride, "hcas", "mlx5_0")

	rdmaAtt, err := svc.Attach(rdma.ID, AttachmentSpec{NodeUUID: node.UUID, Overrides: Overrides{HCAs: []string{"mlx5_0", "mlx5_0", "mlx5_1"}}})
	if err != nil {
		t.Fatalf("attach rdma: %+v", err)
	}
	if got, want := store.DecodeStringList(rdmaAtt.HCAs), []string{"mlx5_0", "mlx5_1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hcas = %v, want %v", got, want)
	}

	_, err = svc.Attach(eth.ID, AttachmentSpec{NodeUUID: "does-not-exist"})
	assertInvalid(t, err, InvalidTarget, "", "does-not-exist")

	if _, err := svc.Attach(999999, AttachmentSpec{NodeUUID: node.UUID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown network err = %+v, want ErrNotFound", err)
	}
}

func TestAttachToLabel(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "label-net", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}

	att, err := svc.Attach(eth.ID, AttachmentSpec{LabelKey: "pool", LabelValue: "a"})
	if err != nil {
		t.Fatalf("attach: %+v", err)
	}
	if att.NodeID != 0 || att.LabelKey != "pool" || att.LabelValue != "a" {
		t.Fatalf("attachment = %+v", att)
	}

	_, err = svc.Attach(eth.ID, AttachmentSpec{LabelKey: "pool2", Overrides: Overrides{Interface: "bond0"}})
	assertInvalid(t, err, InvalidOverride, "target", "label")

	_, err = svc.Attach(eth.ID, AttachmentSpec{LabelKey: "bad key"})
	if ierr := assertInvalid(t, err, InvalidTarget, "", ""); ierr.Detail == "" {
		t.Fatalf("invalid label key error carries no detail: %+v", ierr)
	}

	_, err = svc.Attach(eth.ID, AttachmentSpec{})
	if ierr := assertInvalid(t, err, InvalidTarget, "", ""); ierr.Detail != "attach to exactly one of node_uuid or label_key" {
		t.Fatalf("detail = %q", ierr.Detail)
	}

	_, err = svc.Attach(eth.ID, AttachmentSpec{NodeUUID: "x", LabelKey: "y"})
	assertInvalid(t, err, InvalidTarget, "", "")
}

func TestUpdateOverrides(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "ov", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	node, err := st.RegisterNode("uuid-ov", store.HashToken("o"), "o")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	labelAtt, err := svc.Attach(eth.ID, AttachmentSpec{LabelKey: "pool", LabelValue: "a"})
	if err != nil {
		t.Fatalf("attach label: %+v", err)
	}
	nodeAtt, err := svc.Attach(eth.ID, AttachmentSpec{NodeUUID: node.UUID})
	if err != nil {
		t.Fatalf("attach node: %+v", err)
	}

	_, err = svc.UpdateOverrides(eth.ID, labelAtt.ID, Overrides{})
	assertInvalid(t, err, InvalidOverride, "target", "label")

	if _, err := svc.UpdateOverrides(eth.ID, 999999, Overrides{}); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("unknown attachment err = %+v, want ErrAttachmentNotFound", err)
	}

	updated, err := svc.UpdateOverrides(eth.ID, nodeAtt.ID, Overrides{Interface: "eth1", IP: "10.0.0.5"})
	if err != nil || updated.Interface != "eth1" || updated.IP != "10.0.0.5" {
		t.Fatalf("update overrides = %+v, %v", updated, err)
	}

	_, err = svc.UpdateOverrides(eth.ID, nodeAtt.ID, Overrides{Interface: "bad iface"})
	assertInvalid(t, err, InvalidOverride, "interface", "bad iface")

	managementOnly := New(st, ManagementOnly{})
	if _, err := managementOnly.UpdateOverrides(eth.ID, nodeAtt.ID, Overrides{}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("update overrides err = %+v, want ErrDedicatedUnavailable", err)
	}
}

func TestDetach(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "det", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	att, err := svc.Attach(eth.ID, AttachmentSpec{LabelKey: "pool", LabelValue: "a"})
	if err != nil {
		t.Fatalf("attach: %+v", err)
	}
	if err := svc.Detach(eth.ID, att.ID); err != nil {
		t.Fatalf("detach: %+v", err)
	}
	if err := svc.Detach(eth.ID, att.ID); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("second detach err = %+v, want ErrAttachmentNotFound", err)
	}
	if err := svc.Detach(eth.ID, 999999); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("unknown attachment err = %+v, want ErrAttachmentNotFound", err)
	}

	managementOnly := New(st, ManagementOnly{})
	if err := managementOnly.Detach(eth.ID, att.ID); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("detach err = %+v, want ErrDedicatedUnavailable", err)
	}
}
