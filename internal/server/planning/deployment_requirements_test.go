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

// Reading a deployment's requirements back as a request and stating that
// request again gives the requirements it was read from: a manual pin as the
// manual node, a pool as stated, the vendor as the target vendor.
func TestRequirementsOfDeploymentRoundTrips(t *testing.T) {
	for name, c := range map[string]struct {
		reqs    nodelabel.Requirements
		perRank bool
		manual  string
	}{
		"manual pin":       {nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}, nodelabel.KeyNode: {"n1"}, "pool": {"a"}}, false, "n1"},
		"automatic pool":   {nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}, nodelabel.KeyNode: {"n1", "n2"}}, false, ""},
		"labels only":      {nodelabel.Requirements{"pool": {"a", "b"}}, false, ""},
		"multi-node ranks": {nodelabel.Requirements{nodelabel.KeyVendor: {"ascend"}, "zone": {"z"}}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			request := RequirementsOfDeployment(c.reqs, c.perRank)
			if request.ManualNode != c.manual {
				t.Fatalf("manual node = %q, want %q", request.ManualNode, c.manual)
			}
			if _, stated := request.Stated[nodelabel.KeyVendor]; stated {
				t.Fatalf("the vendor stays a requirement: %v", request.Stated)
			}
			again, err := RequestRequirements(request)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := nodelabel.NormalizeRequirements(c.reqs)
			if len(again) != len(want) {
				t.Fatalf("round trip = %v, want %v", again, want)
			}
			for key, values := range want {
				if got := again[key]; len(got) != len(values) {
					t.Fatalf("round trip %s = %v, want %v", key, got, values)
				}
			}
		})
	}
}
