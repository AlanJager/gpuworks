package store

import (
	"reflect"
	"testing"
)

func TestExistingNodeUUIDsNamesOnlyNodesStillThere(t *testing.T) {
	s := newTestStore(t)
	for _, uuid := range []string{"a", "b"} {
		if _, err := s.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteNode("b"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ExistingNodeUUIDs([]string{"a", "b", "never"})
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"a": true}) {
		t.Fatalf("existing = %v, %v", got, err)
	}
	if got, err := s.ExistingNodeUUIDs(nil); err != nil || len(got) != 0 {
		t.Fatalf("none asked = %v, %v", got, err)
	}
}
