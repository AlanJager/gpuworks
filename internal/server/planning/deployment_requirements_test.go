package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func TestNamedNodesAndOnlyNamesNodes(t *testing.T) {
	named := nodelabel.Requirements{nodelabel.KeyNode: {"a", "b"}}
	if got := NamedNodes(named); len(got) != 2 || !OnlyNamesNodes(named) {
		t.Fatalf("named: %v only=%v", got, OnlyNamesNodes(named))
	}
	mixed := nodelabel.Requirements{nodelabel.KeyNode: {"a"}, "pool": {"x"}}
	if len(NamedNodes(mixed)) != 1 || OnlyNamesNodes(mixed) {
		t.Fatal("a node requirement with another key is not node-only")
	}
	labels := nodelabel.Requirements{"pool": {"x"}}
	if NamedNodes(labels) != nil || OnlyNamesNodes(labels) || OnlyNamesNodes(nil) {
		t.Fatal("requirements naming no node")
	}
}
