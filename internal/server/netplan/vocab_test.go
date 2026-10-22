package netplan

import (
	"errors"
	"reflect"
	"testing"
)

func TestCanonicalRoles(t *testing.T) {
	// Deduplicated and returned in Roles display order, not sorted or input
	// order.
	got, err := CanonicalRoles([]string{RoleServing, RoleCompute, RoleServing, RoleStorageSync})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{RoleStorageSync, RoleCompute, RoleServing} // Roles order
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CanonicalRoles = %v, want %v", got, want)
	}
	// Empty input: non-nil, empty.
	if got, err := CanonicalRoles(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("CanonicalRoles(nil) = %#v, %v", got, err)
	}
	// Surrounding spaces are trimmed before validating.
	if got, err := CanonicalRoles([]string{" compute "}); err != nil || !reflect.DeepEqual(got, []string{RoleCompute}) {
		t.Fatalf("CanonicalRoles(\" compute \") = %#v, %v", got, err)
	}
}

func TestCanonicalRolesUnknownRole(t *testing.T) {
	_, err := CanonicalRoles([]string{"bogus"})
	if !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("unknown role err = %v", err)
	}
	var ure *UnknownRoleError
	if !errors.As(err, &ure) || ure.Role != "bogus" {
		t.Fatalf("errors.As = %#v, %v", ure, err)
	}
	if ure.Error() == "" {
		t.Fatal("empty message")
	}
	// The bad role name in the error is trimmed too.
	_, err = CanonicalRoles([]string{"  bogus  "})
	if !errors.As(err, &ure) || ure.Role != "bogus" {
		t.Fatalf("trimmed errors.As = %#v, %v", ure, err)
	}
}

func TestValidType(t *testing.T) {
	for _, typ := range Types {
		if !ValidType(typ) {
			t.Fatalf("ValidType(%q) = false", typ)
		}
	}
	if ValidType("bogus") {
		t.Fatal("ValidType accepted an unknown type")
	}
	if ValidType("") {
		t.Fatal("ValidType accepted an empty type")
	}
}

func TestInternalTestsCoverNodeToNodeRoles(t *testing.T) {
	for role, want := range map[string]bool{RoleStorageSync: true, RoleCompute: true, RoleServing: false, "": false} {
		if got := TestedRole(role); got != want {
			t.Fatalf("TestedRole(%q) = %v", role, got)
		}
	}
	if (Network{Roles: []string{RoleServing}}).Testable() {
		t.Fatal("a serving-only network is testable")
	}
	if !(Network{Roles: []string{RoleServing, RoleCompute}}).Testable() || !Management().Testable() {
		t.Fatal("a compute network is not testable")
	}
}
