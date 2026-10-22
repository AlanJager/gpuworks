package planning

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// labelled renders the generated labels as "key=value" lines so a test can
// state the whole expected set at once.
func labelled(labels []SystemLabel) string {
	var out []string
	for _, label := range labels {
		out = append(out, label.Key+"="+label.Value)
	}
	return strings.Join(out, "\n")
}

func displayOf(labels []SystemLabel, key, value string) (string, string) {
	for _, label := range labels {
		if label.Key == key && label.Value == value {
			return label.Display, label.DisplayCode
		}
	}
	return "", ""
}

// A node carries one label per fact the platform knows about it, and a value
// per card model it has. The values are the stored facts, not what a probe
// happened to report: an NVIDIA card stores no arch id, and its family comes
// from the compute capability.
func TestSystemLabelsDescribeTheNodesOwnFacts(t *testing.T) {
	node := &store.Node{
		ID: 7, UUID: "node-uuid", Name: "gpu3", Arch: "amd64", GPUVendor: "nvidia",
		GPUs: []store.GPU{
			{Index: 0, Name: "GeForce RTX 4090", ComputeCapability: "8.9"},
			{Index: 1, Name: "GeForce RTX 4090", ComputeCapability: "8.9"},
			{Index: 2, Name: "Quadro RTX 8000", ComputeCapability: "7.5"},
		},
	}
	want := strings.Join([]string{
		"gpuworks.ai/cpu-arch=amd64",
		"gpuworks.ai/gpu-arch=ada",
		"gpuworks.ai/gpu-arch=turing",
		"gpuworks.ai/gpu-model=GeForce RTX 4090",
		"gpuworks.ai/gpu-model=Quadro RTX 8000",
		"gpuworks.ai/node=node-uuid",
		"gpuworks.ai/vendor=nvidia",
	}, "\n")
	if got := labelled(SystemLabels(node, NodeFacts{})); got != want {
		t.Fatalf("labels =\n%s\nwant\n%s", got, want)
	}

	labels := SystemLabels(node, NodeFacts{})
	if display, code := displayOf(labels, nodelabel.KeyNode, "node-uuid"); display != "gpu3" || code != "" {
		t.Fatalf("node display = %q/%q, want the node name and no code", display, code)
	}
	if display, code := displayOf(labels, nodelabel.KeyVendor, "nvidia"); display != "NVIDIA" || code != "common:vendor.nvidia" {
		t.Fatalf("vendor display = %q/%q", display, code)
	}
	if display, code := displayOf(labels, nodelabel.KeyGPUArch, "ada"); display != "Ada" || code != "" {
		t.Fatalf("gpu-arch display = %q/%q, want the brand spelling and no code", display, code)
	}
	if display, _ := displayOf(labels, nodelabel.KeyGPUModel, "GeForce RTX 4090"); display != "GeForce RTX 4090" {
		t.Fatalf("gpu-model display = %q", display)
	}
}

// A card the platform cannot place an architecture for still contributes its
// model: the selector that can be written is the one that is offered.
func TestSystemLabelsLeaveOutWhatTheyCannotDerive(t *testing.T) {
	node := &store.Node{
		ID: 1, UUID: "u", Name: "n",
		GPUs: []store.GPU{{Index: 0, Name: "Some Accelerator X1", Vendor: "mystery"}},
	}
	got := labelled(SystemLabels(node, NodeFacts{}))
	want := "gpuworks.ai/gpu-model=Some Accelerator X1\ngpuworks.ai/node=u"
	if got != want {
		t.Fatalf("labels =\n%s\nwant\n%s", got, want)
	}
}

// An Ascend node's architecture comes from the card's own identity, and its
// model name is the normalized one that is stored.
func TestSystemLabelsReadAscendCardsByIdentity(t *testing.T) {
	node := &store.Node{
		ID: 2, UUID: "u", Name: "910c_1", Arch: "arm64", GPUVendor: "ascend",
		GPUs: []store.GPU{{Index: 0, Name: "910C", ArchID: "910C"}},
	}
	labels := SystemLabels(node, NodeFacts{})
	if got := SystemLabelValues(labels, nodelabel.KeyGPUArch); len(got) != 1 || got[0] != "910c" {
		t.Fatalf("gpu-arch = %v, want [910c]", got)
	}
	if display, _ := displayOf(labels, nodelabel.KeyGPUArch, "910c"); display != "910C" {
		t.Fatalf("910c display = %q, want 910C", display)
	}
	if got := SystemLabelValues(labels, nodelabel.KeyVendor); len(got) != 1 || got[0] != "ascend" {
		t.Fatalf("vendor = %v", got)
	}
}

// The network labels name the network carrying each role by its id, with the
// network's current name to show. A role the management network carries is
// not an id: it says the node has nothing dedicated for that role, which is
// a thing a selector can ask for.
func TestSystemLabelsNameTheNetworkCarryingEachRole(t *testing.T) {
	node := &store.Node{ID: 3, UUID: "u", Name: "n"}
	resolved := netplan.Resolution{
		{NodeID: 3, Role: netplan.RoleCompute, Network: netplan.Network{ID: 12, Name: "参数网-1"}, Source: netplan.SourceNode},
		{NodeID: 3, Role: netplan.RoleStorageSync, Source: netplan.SourceManagement},
	}
	labels := SystemLabels(node, NodeFacts{Networks: resolved})
	if got := SystemLabelValues(labels, nodelabel.KeyComputeNetwork); len(got) != 1 || got[0] != "12" {
		t.Fatalf("compute-network = %v, want the network id", got)
	}
	if display, code := displayOf(labels, nodelabel.KeyComputeNetwork, "12"); display != "参数网-1" || code != "" {
		t.Fatalf("compute-network display = %q/%q, want the network name", display, code)
	}
	if got := SystemLabelValues(labels, nodelabel.KeyStorageSyncNetwork); len(got) != 1 || got[0] != "management" {
		t.Fatalf("storage-sync-network = %v, want management", got)
	}
	if _, code := displayOf(labels, nodelabel.KeyStorageSyncNetwork, "management"); code != "networks:management.name" {
		t.Fatalf("management display code = %q", code)
	}
	// Serving reaches this node through nothing at all, so it carries no
	// label - the absence is what "has no serving network" looks like.
	if got := SystemLabelValues(labels, nodelabel.KeyServingNetwork); got != nil {
		t.Fatalf("serving-network = %v, want none", got)
	}
}

// Renaming a network changes what the label shows without changing what it
// matches: the value is the id, so a selector written before the rename
// still selects the same network.
func TestSystemLabelNetworkValuesSurviveARename(t *testing.T) {
	node := &store.Node{ID: 4, UUID: "u", Name: "n"}
	before := SystemLabels(node, NodeFacts{Networks: netplan.Resolution{
		{NodeID: 4, Role: netplan.RoleCompute, Network: netplan.Network{ID: 9, Name: "old"}, Source: netplan.SourceNode},
	}})
	after := SystemLabels(node, NodeFacts{Networks: netplan.Resolution{
		{NodeID: 4, Role: netplan.RoleCompute, Network: netplan.Network{ID: 9, Name: "new"}, Source: netplan.SourceNode},
	}})
	if labelled(before) != labelled(after) {
		t.Fatalf("a rename changed the matched values: %q vs %q", labelled(before), labelled(after))
	}
	if display, _ := displayOf(after, nodelabel.KeyComputeNetwork, "9"); display != "new" {
		t.Fatalf("display after the rename = %q", display)
	}
}

// Every generated key sits in the namespace users are kept out of, so a
// selector's author can tell a platform fact from a written one.
func TestSystemLabelKeysAreAllReserved(t *testing.T) {
	node := &store.Node{
		ID: 5, UUID: "u", Name: "n", Arch: "amd64", GPUVendor: "nvidia",
		GPUs: []store.GPU{{Index: 0, Name: "GeForce RTX 4090", ComputeCapability: "8.9"}},
	}
	labels := SystemLabels(node, NodeFacts{Networks: netplan.Resolution{
		{NodeID: 5, Role: netplan.RoleServing, Source: netplan.SourceManagement},
	}})
	if len(labels) == 0 {
		t.Fatal("no labels generated")
	}
	for _, label := range labels {
		if !nodelabel.IsSystemKey(label.Key) {
			t.Fatalf("key %q is not a declared system key, so no requirement may name it", label.Key)
		}
		if err := nodelabel.ValidateWritten(map[string]string{label.Key: label.Value}); err == nil {
			t.Fatalf("a user could write %q", label.Key)
		}
		if label.Display == "" {
			t.Fatalf("%s=%s has nothing to show", label.Key, label.Value)
		}
	}
}

func TestSystemLabelsOfNoNode(t *testing.T) {
	if got := SystemLabels(nil, NodeFacts{}); got != nil {
		t.Fatalf("labels of no node = %v", got)
	}
}
