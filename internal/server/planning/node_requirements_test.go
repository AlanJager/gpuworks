package planning

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func requirementNode() *store.Node {
	node := testNode("node-a", map[string]string{"pool": "inference", "zone": "a", "empty": ""})
	node.ID, node.Name, node.Arch, node.GPUVendor = 5, "gpu3", "amd64", "nvidia"
	node.GPUs = []store.GPU{
		{Index: 0, Name: "GeForce RTX 2080 Ti", ComputeCapability: "7.5"},
		{Index: 1, Name: "Quadro RTX 8000", ComputeCapability: "7.5"},
	}
	return node
}

// Keys must all hold; the values of one key are alternatives. Written keys
// are matched against the node's labels, system keys against the labels the
// platform generates for it.
func TestMatchRequirements(t *testing.T) {
	node := requirementNode()
	resolved := netplan.Resolution{{NodeID: 5, Role: netplan.RoleCompute, Source: netplan.SourceManagement}}
	for name, c := range map[string]struct {
		reqs  nodelabel.Requirements
		unmet string
	}{
		"none":                           {nil, ""},
		"written label":                  {nodelabel.Requirements{"pool": {"inference"}}, ""},
		"written label, one of two":      {nodelabel.Requirements{"zone": {"b", "a"}}, ""},
		"an empty written value present": {nodelabel.Requirements{"empty": {""}}, ""},
		"written label differs":          {nodelabel.Requirements{"zone": {"b"}}, "zone"},
		"written label absent":           {nodelabel.Requirements{"missing": {""}}, "missing"},
		"one card of a mixed node":       {nodelabel.Requirements{nodelabel.KeyGPUModel: {"Quadro RTX 8000"}}, ""},
		"no such card":                   {nodelabel.Requirements{nodelabel.KeyGPUModel: {"H100"}}, nodelabel.KeyGPUModel},
		"its own node and vendor":        {nodelabel.Requirements{nodelabel.KeyNode: {"x", "node-a"}, nodelabel.KeyVendor: {"nvidia"}}, ""},
		"another vendor":                 {nodelabel.Requirements{nodelabel.KeyVendor: {"ascend"}}, nodelabel.KeyVendor},
		"management carries compute":     {nodelabel.Requirements{nodelabel.KeyComputeNetwork: {nodelabel.ManagementNetwork}}, ""},
		"a deleted node's pin":           {nodelabel.Requirements{nodelabel.KeyNode: {"#9"}}, nodelabel.KeyNode},
		// Keys are checked in order, so the first unmet one is reported.
		"first unmet in key order": {nodelabel.Requirements{"zone": {"b"}, nodelabel.KeyVendor: {"ascend"}}, nodelabel.KeyVendor},
	} {
		err := MatchRequirements(node, NodeFacts{Networks: resolved}, c.reqs)
		var unmet *UnmetRequirement
		switch {
		case c.unmet == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.unmet != "" && (!errors.As(err, &unmet) || unmet.Key != c.unmet):
			t.Errorf("%s: err = %v, want %s unmet", name, err, c.unmet)
		}
	}
}

// Nothing unreadable is ever read as a match.
func TestMatchRequirementsFailsClosed(t *testing.T) {
	node := requirementNode()
	network := nodelabel.Requirements{nodelabel.KeyComputeNetwork: {nodelabel.ManagementNetwork}}
	if err := MatchRequirements(node, NodeFacts{}, network); err == nil {
		t.Fatal("a network requirement matched without a resolution")
	}
	node.Labels = "not json"
	if err := MatchRequirements(node, NodeFacts{}, nodelabel.Requirements{"pool": {"inference"}}); err == nil {
		t.Fatal("unreadable labels matched a written requirement")
	}
	// A system requirement does not need the written labels.
	if err := MatchRequirements(node, NodeFacts{}, nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}}); err != nil {
		t.Fatalf("vendor requirement read the written labels: %v", err)
	}
	if err := MatchRequirements(nil, NodeFacts{}, nodelabel.Requirements{"pool": {"a"}}); err == nil {
		t.Fatal("requirements matched no node")
	}
	if !RequirementsNeedNetworks(network) || RequirementsNeedNetworks(nodelabel.Requirements{"pool": {"a"}}) {
		t.Fatal("RequirementsNeedNetworks")
	}
}

func TestRequestRequirements(t *testing.T) {
	for name, c := range map[string]struct {
		in   RequirementsRequest
		want nodelabel.Requirements
	}{
		"automatic, nothing stated": {RequirementsRequest{}, nodelabel.Requirements{}},
		"manual pins its node": {
			RequirementsRequest{ManualNode: "n-1", TargetVendor: "nvidia", Stated: map[string][]string{"pool": {"b", "a"}}},
			nodelabel.Requirements{nodelabel.KeyNode: {"n-1"}, nodelabel.KeyVendor: {"nvidia"}, "pool": {"a", "b"}},
		},
		"manual within a stated node list": {
			RequirementsRequest{ManualNode: "n-1", Stated: map[string][]string{nodelabel.KeyNode: {"n-2", "n-1"}}},
			nodelabel.Requirements{nodelabel.KeyNode: {"n-1", "n-2"}},
		},
		"target vendor agrees with the stated one": {
			RequirementsRequest{TargetVendor: "Ascend", Stated: map[string][]string{nodelabel.KeyVendor: {"ascend"}}},
			nodelabel.Requirements{nodelabel.KeyVendor: {"ascend"}},
		},
		"per-rank requests take labels": {
			RequirementsRequest{PerRank: true, Stated: map[string][]string{"pool": {"a"}}},
			nodelabel.Requirements{"pool": {"a"}},
		},
	} {
		got, err := RequestRequirements(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, %v; want %v", name, got, err, c.want)
		}
	}
	for name, in := range map[string]RequirementsRequest{
		"manual outside a stated node list": {ManualNode: "n-3", Stated: map[string][]string{nodelabel.KeyNode: {"n-1"}}},
		"vendors disagree":                  {TargetVendor: "nvidia", Stated: map[string][]string{nodelabel.KeyVendor: {"ascend"}}},
		"unknown target vendor":             {TargetVendor: "intel"},
		"per-rank names nodes":              {PerRank: true, Stated: map[string][]string{nodelabel.KeyNode: {"n-1"}}},
		"misspelt system key":               {Stated: map[string][]string{nodelabel.Prefix + "gpu_model": {"x"}}},
	} {
		_, err := RequestRequirements(in)
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != messages.ErrorNodeRequirementsInvalid {
			t.Errorf("%s: err = %v, want %s", name, err, messages.ErrorNodeRequirementsInvalid)
		}
	}
}

func TestPlacementStrategyAndVendor(t *testing.T) {
	for stated, want := range map[string]store.PlacementStrategy{"": store.PlacementSpread, "spread": store.PlacementSpread, "binpack": store.PlacementBinpack} {
		if got, err := PlacementStrategyOf(stated); err != nil || got != want {
			t.Errorf("%q = %q, %v", stated, got, err)
		}
	}
	if _, err := PlacementStrategyOf("Binpack"); err == nil || !strings.Contains(err.Error(), "spread or binpack") {
		t.Errorf("a strategy in another case was accepted: %v", err)
	}

	node := requirementNode()
	got := WithNodeVendor(nodelabel.Requirements{"pool": {"a"}}, node)
	if !reflect.DeepEqual(got, nodelabel.Requirements{"pool": {"a"}, nodelabel.KeyVendor: {"nvidia"}}) {
		t.Fatalf("filled = %v", got)
	}
	stated := nodelabel.Requirements{nodelabel.KeyVendor: {"ascend"}}
	if got := WithNodeVendor(stated, node); !reflect.DeepEqual(got, stated) {
		t.Fatalf("a stated vendor was replaced: %v", got)
	}
	node.GPUVendor = ""
	if got := WithNodeVendor(nil, node); len(got) != 0 {
		t.Fatalf("a node with no vendor added one: %v", got)
	}
	if RequirementVendor(stated) != "ascend" || RequirementVendor(nil) != "" {
		t.Fatal("RequirementVendor")
	}
	if !IsManualPlacement(nodelabel.Requirements{nodelabel.KeyNode: {"n"}}) || IsManualPlacement(stated) {
		t.Fatal("IsManualPlacement")
	}
}
