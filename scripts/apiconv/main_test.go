package main

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestCheckReportsEachRuleOnFixture runs the checker on a fixture package
// holding one response per rule and asserts the exact findings: a bare slice
// is an envelope violation ([]byte is not), untagged and PascalCase fields
// and a non-snake_case map-literal key are snake_case violations, fields of a
// type with its own MarshalJSON are not inspected, and any/json.RawMessage
// fields are counted as opaque.
func TestCheckReportsEachRuleOnFixture(t *testing.T) {
	found, opaque, err := check(".", "./testdata/violations")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range found {
		if filepath.Base(v.pos.Filename) != "violations.go" || v.pos.Line == 0 {
			t.Fatalf("violation without a fixture position: %+v", v)
		}
		got = append(got, v.rule+": "+v.what)
	}
	sort.Strings(got)
	want := []string{
		`envelope: response is a bare []violations.Row; answer {"items": [...]} (dto.List)`,
		`snake_case: field violations.Bad.Pascal encodes as "PascalCase"`,
		`snake_case: field violations.Bad.Untagged encodes as "Untagged"`,
		`snake_case: map key "BadKey"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("violations\n got %q\nwant %q", got, want)
	}
	wantOpaque := map[string]bool{"violations.Opaque.Extra": true, "violations.Opaque.Raw": true}
	if !reflect.DeepEqual(opaque, wantOpaque) {
		t.Fatalf("opaque = %v, want %v", opaque, wantOpaque)
	}
}

// TestCheckAcceptsConformingFixture: envelopes, nested snake_case structs and
// snake_case map literals produce nothing.
func TestCheckAcceptsConformingFixture(t *testing.T) {
	found, opaque, err := check(".", "./testdata/clean")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 || len(opaque) != 0 {
		t.Fatalf("clean fixture: violations %+v, opaque %v", found, opaque)
	}
}

// TestCheckFailsOnPackageThatDoesNotLoad: a pattern that matches nothing
// loadable is an error, not a silent pass.
func TestCheckFailsOnPackageThatDoesNotLoad(t *testing.T) {
	if _, _, err := check(".", "./testdata/missing"); err == nil {
		t.Fatal("check on a missing package should fail")
	}
}
