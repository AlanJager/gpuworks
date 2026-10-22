package store

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

func managementRolesOf(t *testing.T, s *Store, uuid string) []string {
	t.Helper()
	n, err := s.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return DecodeStringList(n.ManagementRolesOff)
}

// addRole is a next function that switches one role off.
func addRole(role string) func([]string) []string {
	return func(current []string) []string {
		for _, r := range current {
			if r == role {
				return current
			}
		}
		return append(append([]string(nil), current...), role)
	}
}

func TestUpdateManagementRolesOffWritesOnlyChangedNodes(t *testing.T) {
	s := newTestStore(t)
	for _, uuid := range []string{"a", "b", "c"} {
		if _, err := s.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetNodeManagementRolesOff("b", []string{"compute"}); err != nil {
		t.Fatal(err)
	}
	sub := s.Bus.Subscribe(16, "node")
	defer sub.Close()

	// b already has the role off; a gains it; c is not named. a is named twice.
	n, err := s.UpdateManagementRolesOff([]string{"a", "b", "a"}, addRole("compute"))
	if err != nil || n != 1 {
		t.Fatalf("updated %d, %v", n, err)
	}
	if got := managementRolesOf(t, s, "a"); !reflect.DeepEqual(got, []string{"compute"}) {
		t.Fatalf("a = %v", got)
	}
	if got := managementRolesOf(t, s, "c"); len(got) != 0 {
		t.Fatalf("c = %v", got)
	}
	var keys []string
	timeout := time.After(2 * time.Second)
	for len(keys) < 1 {
		select {
		case ev := <-sub.C():
			if ev.Reason == "management_roles" {
				keys = append(keys, ev.Key)
			}
		case <-timeout:
			t.Fatalf("published %v, want a", keys)
		}
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"a"}) {
		t.Fatalf("published %v", keys)
	}

	// The same set in another order is not a change.
	if err := s.SetNodeManagementRolesOff("a", []string{"serving", "compute"}); err != nil {
		t.Fatal(err)
	}
	reorder := func([]string) []string { return []string{"compute", "serving"} }
	if n, err := s.UpdateManagementRolesOff([]string{"a"}, reorder); err != nil || n != 0 {
		t.Fatalf("reorder updated %d, %v", n, err)
	}
}

func TestUpdateManagementRolesOffIsAllOrNothing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("a", HashToken("a"), "a"); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpdateManagementRolesOff([]string{"a", "gone"}, addRole("serving"))
	var missing *ManagementRolesNotFoundError
	if !errors.As(err, &missing) || missing.UUID != "gone" || !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if got := managementRolesOf(t, s, "a"); len(got) != 0 {
		t.Fatalf("a changed although the batch failed: %v", got)
	}
	if err := s.SetNodeManagementRolesOff("gone", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("single missing node err = %v", err)
	}
}
