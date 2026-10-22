package store

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func TestNodeLabelsReplace(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RegisterNode("labels", HashToken("labels"), "worker"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"pool": "inference", "resource_group": "gpu", "empty": ""}
	n, err := st.SetNodeLabels("labels", want)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := NodeLabels(n.Labels)
	if err != nil || !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels=%v err=%v", labels, err)
	}
	if _, err = st.SetNodeLabels("labels", map[string]string{"bad key": "value"}); err == nil {
		t.Fatal("invalid key accepted")
	}
	n, _ = st.NodeByUUID("labels")
	labels, _ = NodeLabels(n.Labels)
	if labels["pool"] != "inference" {
		t.Fatal("invalid update mutated labels")
	}
	if _, err = st.SetNodeLabels("missing", want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
	n, err = st.SetNodeLabels("labels", map[string]string{})
	if err != nil || n.Labels != "{}" {
		t.Fatalf("clear: %v %v", n, err)
	}
}

// The platform's own namespace is closed to written labels, in any letter
// case and at any depth, so a generated label can never be shadowed by one
// an operator wrote. Everything outside it stays writable.
func TestNodeLabelsRefuseTheReservedNamespace(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RegisterNode("reserved", HashToken("reserved"), "worker"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"gpuworks.ai/node",
		"gpuworks.ai/anything",
		"GPUWORKS.AI/node",
		"rack.gpuworks.ai/row",
		"a.b.gpuworks.ai/x",
	} {
		if err := nodelabel.ValidateWritten(map[string]string{key: "v"}); err == nil {
			t.Errorf("%q accepted", key)
		}
		if _, err := st.SetNodeLabels("reserved", map[string]string{key: "v"}); err == nil {
			t.Errorf("%q written to a node", key)
		}
	}
	// Near misses are someone else's namespace, not ours.
	for _, key := range []string{
		"gpuworks.ai",      // no path, so no domain at all
		"gpuworks-ai/node", // a dash, not a dot
		"notgpuworks.ai/x",
		"gpuworks.ai.example.com/x",
		"example.com/gpuworks.ai",
	} {
		if err := nodelabel.ValidateWritten(map[string]string{key: "v"}); err != nil {
			t.Errorf("%q refused: %v", key, err)
		}
	}
	n, err := st.SetNodeLabels("reserved", map[string]string{"pool": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if labels, _ := NodeLabels(n.Labels); labels["pool"] != "a" {
		t.Fatalf("labels = %v, want the written one kept", labels)
	}
}
