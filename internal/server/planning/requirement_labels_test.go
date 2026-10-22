package planning

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Each value reads as the node pages show it - a node or network by name -
// with how many nodes carry it now; one nothing carries (a deleted node or
// network, a label no node has) stays listed as itself with no nodes.
func TestRequirementLabels(t *testing.T) {
	a := testNode("u-a", map[string]string{"pool": "inference"})
	a.ID, a.Name, a.GPUVendor = 1, "gpu-a", "nvidia"
	b := testNode("u-b", map[string]string{"pool": "training"})
	b.ID, b.Name, b.GPUVendor = 2, "gpu-b", "nvidia"
	broken := &store.Node{ID: 3, UUID: "u-c", Name: "gpu-c", GPUVendor: "nvidia", Labels: "{"}
	resolved := netplan.Resolution{
		{NodeID: 1, Role: netplan.RoleCompute, Network: netplan.Network{ID: 12, Name: "参数网-1"}, Source: netplan.SourceNode},
		{NodeID: 2, Role: netplan.RoleCompute, Network: netplan.Network{ID: 12, Name: "参数网-1"}, Source: netplan.SourceNode},
	}
	got := RequirementLabels(nodelabel.Requirements{
		nodelabel.KeyNode:           {"u-a", "#9"},
		nodelabel.KeyVendor:         {"nvidia"},
		nodelabel.KeyComputeNetwork: {"12", "40"},
		"pool":                      {"inference", "nowhere"},
	}, []*store.Node{a, b, broken}, NodeFacts{Networks: resolved})
	want := []RequirementLabel{
		{Key: nodelabel.KeyComputeNetwork, Value: "12", Display: "参数网-1", Nodes: 2},
		{Key: nodelabel.KeyComputeNetwork, Value: "40", Display: "40", Nodes: 0},
		{Key: nodelabel.KeyNode, Value: "#9", Display: "#9", Nodes: 0},
		{Key: nodelabel.KeyNode, Value: "u-a", Display: "gpu-a", Nodes: 1},
		{Key: nodelabel.KeyVendor, Value: "nvidia", Display: "NVIDIA", DisplayCode: "common:vendor.nvidia", Nodes: 3},
		{Key: "pool", Value: "inference", Display: "inference", Nodes: 1},
		{Key: "pool", Value: "nowhere", Display: "nowhere", Nodes: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels =\n%+v\nwant\n%+v", got, want)
	}
	if RequirementLabels(nil, []*store.Node{a}, NodeFacts{}) != nil {
		t.Fatal("no requirements, no labels")
	}
}

// An edit keeps the vendor the deployment's image is for: unstated it is
// carried over, the same is accepted, another is refused.
func TestEditedRequirements(t *testing.T) {
	stored := nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}, nodelabel.KeyNode: {"n-1"}}
	got, err := EditedRequirements(map[string][]string{"pool": {"b", "a"}}, stored)
	if err != nil {
		t.Fatal(err)
	}
	if want := (nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}, "pool": {"a", "b"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}
	if _, err := EditedRequirements(map[string][]string{nodelabel.KeyVendor: {"nvidia"}}, stored); err != nil {
		t.Fatalf("the same vendor = %v", err)
	}
	var product *messages.ProductError
	for name, stated := range map[string]map[string][]string{
		"another vendor":        {nodelabel.KeyVendor: {"ascend"}},
		"two vendors":           {nodelabel.KeyVendor: {"nvidia", "ascend"}},
		"an empty vendor list":  {nodelabel.KeyVendor: {}},
		"a misspelt system key": {"gpuworks.ai/gpu_model": {"H100"}},
	} {
		if _, err := EditedRequirements(stated, stored); !errors.As(err, &product) || product.Code() != messages.ErrorNodeRequirementsInvalid {
			t.Errorf("%s: err = %v, want %s", name, err, messages.ErrorNodeRequirementsInvalid)
		}
	}
	// A deployment stored without a vendor (it landed on a node reporting
	// none) takes whatever the edit states.
	if got, err := EditedRequirements(map[string][]string{nodelabel.KeyVendor: {"ascend"}}, nil); err != nil || RequirementVendor(got) != "ascend" {
		t.Fatalf("no stored vendor: %v %v", got, err)
	}
}
