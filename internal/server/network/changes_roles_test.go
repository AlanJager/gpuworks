package network

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func rolesOffOf(t *testing.T, st *store.Store, uuid string) []string {
	t.Helper()
	n, err := st.NodeByUUID(uuid)
	if err != nil {
		t.Fatalf("load %s: %+v", uuid, err)
	}
	return store.DecodeStringList(n.ManagementRolesOff)
}

func TestSetManagementRolesOff(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := st.RegisterNode("uuid-roles", store.HashToken("r"), "r")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}

	if err := svc.SetManagementRolesOff(n.UUID, []string{"bogus"}); !errors.Is(err, netplan.ErrUnknownRole) {
		t.Fatalf("unknown role err = %+v, want ErrUnknownRole", err)
	}
	if err := svc.SetManagementRolesOff("does-not-exist", []string{netplan.RoleServing}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node err = %+v, want ErrNotFound", err)
	}

	// Stored in display order regardless of the order passed in.
	if err := svc.SetManagementRolesOff(n.UUID, []string{netplan.RoleServing, netplan.RoleStorageSync}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	want := []string{netplan.RoleStorageSync, netplan.RoleServing}
	if got := rolesOffOf(t, st, n.UUID); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles off = %v, want %v", got, want)
	}
}

func TestUpdateManagementRolesAllowDenyMerge(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	for _, uuid := range []string{"a", "b", "c"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatalf("register %s: %+v", uuid, err)
		}
	}
	if err := svc.SetManagementRolesOff("a", []string{netplan.RoleServing}); err != nil {
		t.Fatalf("set a: %+v", err)
	}
	if err := svc.SetManagementRolesOff("b", []string{netplan.RoleStorageSync}); err != nil {
		t.Fatalf("set b: %+v", err)
	}

	n, err := svc.UpdateManagementRoles([]string{"a", "b", "a"}, []string{netplan.RoleServing}, []string{netplan.RoleCompute})
	if err != nil || n != 2 {
		t.Fatalf("updated %d, %+v, want 2 nodes changed", n, err)
	}
	if got := rolesOffOf(t, st, "a"); !reflect.DeepEqual(got, []string{netplan.RoleCompute}) {
		t.Fatalf("a = %v", got)
	}
	if got := rolesOffOf(t, st, "b"); !reflect.DeepEqual(got, []string{netplan.RoleStorageSync, netplan.RoleCompute}) {
		t.Fatalf("b = %v", got)
	}
	if got := rolesOffOf(t, st, "c"); len(got) != 0 {
		t.Fatalf("c = %v, want untouched", got)
	}

	// Repeating the same batch changes nothing further.
	if n, err := svc.UpdateManagementRoles([]string{"a", "b"}, []string{netplan.RoleServing}, []string{netplan.RoleCompute}); err != nil || n != 0 {
		t.Fatalf("repeat updated %d, %+v, want 0", n, err)
	}
}

func TestUpdateManagementRolesConflictUnknownRoleAndMissingNode(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	if _, err := st.RegisterNode("only", store.HashToken("only"), "only"); err != nil {
		t.Fatalf("register: %+v", err)
	}

	if _, err := svc.UpdateManagementRoles([]string{"only"}, []string{"bogus"}, nil); !errors.Is(err, netplan.ErrUnknownRole) {
		t.Fatalf("unknown allow role err = %+v, want ErrUnknownRole", err)
	}
	if _, err := svc.UpdateManagementRoles([]string{"only"}, nil, []string{"bogus"}); !errors.Is(err, netplan.ErrUnknownRole) {
		t.Fatalf("unknown deny role err = %+v, want ErrUnknownRole", err)
	}
	if _, err := svc.UpdateManagementRoles([]string{"only"}, []string{netplan.RoleServing}, []string{netplan.RoleServing}); !errors.Is(err, ErrManagementRolesConflict) {
		t.Fatalf("conflict err = %+v, want ErrManagementRolesConflict", err)
	}

	_, err := svc.UpdateManagementRoles([]string{"only", "gone"}, nil, []string{netplan.RoleServing})
	var missing *store.ManagementRolesNotFoundError
	if !errors.As(err, &missing) || missing.UUID != "gone" || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing node err = %+v, want *ManagementRolesNotFoundError{UUID: gone}", err)
	}
	if got := rolesOffOf(t, st, "only"); len(got) != 0 {
		t.Fatalf("only changed although the batch failed: %v", got)
	}
}

func TestManagementOnlyStillAppliesManagementRoles(t *testing.T) {
	st := newStore(t)
	svc := New(st, ManagementOnly{})
	if _, err := st.RegisterNode("mo-roles", store.HashToken("mo"), "mo"); err != nil {
		t.Fatalf("register: %+v", err)
	}
	if err := svc.SetManagementRolesOff("mo-roles", []string{netplan.RoleCompute}); err != nil {
		t.Fatalf("set roles off: %+v", err)
	}
	if got := rolesOffOf(t, st, "mo-roles"); !reflect.DeepEqual(got, []string{netplan.RoleCompute}) {
		t.Fatalf("roles off = %v", got)
	}
	if n, err := svc.UpdateManagementRoles([]string{"mo-roles"}, []string{netplan.RoleCompute}, []string{netplan.RoleServing}); err != nil || n != 1 {
		t.Fatalf("update = %d, %+v", n, err)
	}
	if got := rolesOffOf(t, st, "mo-roles"); !reflect.DeepEqual(got, []string{netplan.RoleServing}) {
		t.Fatalf("roles off after update = %v", got)
	}
}
