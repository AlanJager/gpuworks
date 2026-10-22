package topoparse

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 4-GPU box: two NVLink pairs, cross-pair = SYS.
const sample4GPU = "\tGPU0\tGPU1\tGPU2\tGPU3\tCPU Affinity\tNUMA Affinity\n" +
	"GPU0\t X \tNV12\tSYS\tSYS\t0-23\t0\n" +
	"GPU1\tNV12\t X \tSYS\tSYS\t0-23\t0\n" +
	"GPU2\tSYS\tSYS\t X \tNV12\t24-47\t1\n" +
	"GPU3\tSYS\tSYS\tNV12\t X \t24-47\t1\n" +
	"\nLegend:\n  X    = Self\n"

// 8-GPU fully NVLinked box (all off-diagonal NV12).
const sample8GPU = "\tGPU0\tGPU1\tGPU2\tGPU3\tGPU4\tGPU5\tGPU6\tGPU7\tCPU Affinity\tNUMA Affinity\n" +
	"GPU0\t X \tNV12\tNV12\tNV12\tNV12\tNV12\tNV12\tNV12\t0-63\t0\n" +
	"GPU1\tNV12\t X \tNV12\tNV12\tNV12\tNV12\tNV12\tNV12\t0-63\t0\n" +
	"GPU2\tNV12\tNV12\t X \tNV12\tNV12\tNV12\tNV12\tNV12\t0-63\t0\n" +
	"GPU3\tNV12\tNV12\tNV12\t X \tNV12\tNV12\tNV12\tNV12\t0-63\t0\n" +
	"GPU4\tNV12\tNV12\tNV12\tNV12\t X \tNV12\tNV12\tNV12\t64-127\t1\n" +
	"GPU5\tNV12\tNV12\tNV12\tNV12\tNV12\t X \tNV12\tNV12\t64-127\t1\n" +
	"GPU6\tNV12\tNV12\tNV12\tNV12\tNV12\tNV12\t X \tNV12\t64-127\t1\n" +
	"GPU7\tNV12\tNV12\tNV12\tNV12\tNV12\tNV12\tNV12\t X \t64-127\t1\n"

// PCIe-only box: no NVLink at all.
const samplePCIe = "\tGPU0\tGPU1\tGPU2\tGPU3\tCPU Affinity\tNUMA Affinity\n" +
	"GPU0\t X \tPHB\tSYS\tSYS\t0-23\t0\n" +
	"GPU1\tPHB\t X \tSYS\tSYS\t0-23\t0\n" +
	"GPU2\tSYS\tSYS\t X \tPHB\t24-47\t1\n" +
	"GPU3\tSYS\tSYS\tPHB\t X \t24-47\t1\n"

// Box with a NIC row/column present.
const sampleNIC = "\tGPU0\tGPU1\tNIC0\tCPU Affinity\tNUMA Affinity\n" +
	"GPU0\t X \tNV12\tPIX\t0-23\t0\n" +
	"GPU1\tNV12\t X \tPIX\t0-23\t0\n" +
	"NIC0\tPIX\tPIX\t X \t\t\n" +
	"\nLegend:\n"

func TestClassOf(t *testing.T) {
	cases := []struct {
		token string
		class LinkClass
		bonds int
	}{
		{"X", LinkSelf, 0},
		{" X ", LinkSelf, 0},
		{"SYS", LinkSYS, 0},
		{"PHB", LinkPHB, 0},
		{"NODE", LinkNODE, 0},
		{"PXB", LinkPXB, 0},
		{"PIX", LinkPIX, 0},
		{"NV12", LinkNVLink, 12},
		{"NV1", LinkNVLink, 1},
		{"NV", LinkNVLink, 0},
		{"ICN2", LinkNVLink, 2},
		{"???", LinkUnknown, 0},
	}
	for _, c := range cases {
		gotClass, gotBonds := ClassOf(c.token)
		if gotClass != c.class || gotBonds != c.bonds {
			t.Errorf("ClassOf(%q) = (%v, %d), want (%v, %d)",
				c.token, gotClass, gotBonds, c.class, c.bonds)
		}
	}
}

func TestParseAutoPPUICNTopology(t *testing.T) {
	const sample = "         PPU0    PPU1    PPU2    PPU3    NIC0    CPU Affinity    NUMA Affinity\n" +
		" PPU0    X       ICN2    ICN1    SYS     SYS     48-95,144-191   2-3\n" +
		" PPU1    ICN2    X       ICN2    ICN1    SYS     48-95,144-191   2-3\n" +
		" PPU2    ICN1    ICN2    X       ICN1    SYS     48-95,144-191   2-3\n" +
		" PPU3    SYS     ICN1    ICN1    X       SYS     48-95,144-191   2-3\n" +
		" NIC0    SYS     SYS     SYS     SYS     X       0-47,96-143     0-1\n\nLegend:\n"
	topo, err := ParseAuto(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.GPUs) != 4 || topo.GPUs[0].Index != 0 || topo.GPUs[3].Index != 3 {
		t.Fatalf("GPUs = %+v", topo.GPUs)
	}
	if got := topo.Matrix[0][1]; got.Class != LinkNVLink || got.NVLinkBonds != 2 || got.Raw != "ICN2" {
		t.Fatalf("ICN link = %+v", got)
	}
	if len(topo.FastCliques) == 0 {
		t.Fatalf("FastCliques = %+v", topo.FastCliques)
	}
	// A NUMA range is not a single node. Store reconciliation enriches this
	// from each PPU's PCI sysfs NUMA node rather than guessing here.
	if len(topo.NUMAGroups) != 0 {
		t.Fatalf("range NUMA must remain unknown: %+v", topo.NUMAGroups)
	}
}

func TestParseMatrix(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantGPUs    int
		wantCliques [][]int
		wantNUMA    map[int][]int
	}{
		{
			name:        "4gpu two pairs",
			input:       sample4GPU,
			wantGPUs:    4,
			wantCliques: [][]int{{0, 1}, {2, 3}},
			wantNUMA:    map[int][]int{0: {0, 1}, 1: {2, 3}},
		},
		{
			name:        "8gpu fully nvlinked",
			input:       sample8GPU,
			wantGPUs:    8,
			wantCliques: [][]int{{0, 1, 2, 3, 4, 5, 6, 7}},
			wantNUMA:    map[int][]int{0: {0, 1, 2, 3}, 1: {4, 5, 6, 7}},
		},
		{
			name:        "pcie only no nvlink",
			input:       samplePCIe,
			wantGPUs:    4,
			wantCliques: nil,
			wantNUMA:    map[int][]int{0: {0, 1}, 1: {2, 3}},
		},
		{
			name:        "nic row and column",
			input:       sampleNIC,
			wantGPUs:    2,
			wantCliques: [][]int{{0, 1}},
			wantNUMA:    map[int][]int{0: {0, 1}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			topo, err := ParseMatrix(c.input)
			if err != nil {
				t.Fatalf("ParseMatrix error: %v", err)
			}
			if len(topo.GPUs) != c.wantGPUs {
				t.Fatalf("GPUs = %d, want %d", len(topo.GPUs), c.wantGPUs)
			}
			if !reflect.DeepEqual(topo.NVLinkCliques, c.wantCliques) {
				t.Errorf("cliques = %v, want %v", topo.NVLinkCliques, c.wantCliques)
			}
			if !reflect.DeepEqual(topo.FastCliques, c.wantCliques) {
				t.Errorf("fast cliques = %v, want %v", topo.FastCliques, c.wantCliques)
			}
			if !reflect.DeepEqual(topo.NUMAGroups, c.wantNUMA) {
				t.Errorf("NUMAGroups = %v, want %v", topo.NUMAGroups, c.wantNUMA)
			}
		})
	}
}

func TestParseMatrixMatrixContent(t *testing.T) {
	topo, err := ParseMatrix(sample4GPU)
	if err != nil {
		t.Fatal(err)
	}
	if topo.Matrix[0][1].Class != LinkNVLink || topo.Matrix[0][1].NVLinkBonds != 12 {
		t.Errorf("Matrix[0][1] = %+v, want NVLink bonds 12", topo.Matrix[0][1])
	}
	if topo.Matrix[0][2].Class != LinkSYS {
		t.Errorf("Matrix[0][2] = %+v, want SYS", topo.Matrix[0][2])
	}
	if topo.Matrix[0][0].Class != LinkSelf {
		t.Errorf("Matrix[0][0] = %+v, want Self", topo.Matrix[0][0])
	}
	if topo.GPUs[2].CPUAffinity != "24-47" || topo.GPUs[2].NUMANode != 1 {
		t.Errorf("GPU2 affinity = %q numa %d, want 24-47/1",
			topo.GPUs[2].CPUAffinity, topo.GPUs[2].NUMANode)
	}
}

func TestParseAutoAscendHCCSSparseNPUIndexes(t *testing.T) {
	const sample = "\tNPU2\tNPU4\tNPU5\tCPU Affinity\tNUMA Affinity\n" +
		"NPU2\t X \tHCCS\tSYS\t0-31\t0\n" +
		"NPU4\tHCCS\t X \tHCCS\t0-31\t0\n" +
		"NPU5\tSYS\tHCCS\t X \t32-63\t1\n"
	topo, err := ParseAuto(sample)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(topo.GPUs), 3; got != want {
		t.Fatalf("GPUs = %d, want %d: %+v", got, want, topo.GPUs)
	}
	if topo.GPUs[0].Index != 2 || topo.GPUs[1].Index != 4 || topo.GPUs[2].Index != 5 {
		t.Fatalf("sparse indexes not preserved: %+v", topo.GPUs)
	}
	if topo.Matrix[2][4].Class != LinkNVLink || topo.Matrix[4][5].Class != LinkNVLink {
		t.Fatalf("HCCS links not classified as fast: %+v %+v", topo.Matrix[2][4], topo.Matrix[4][5])
	}
	if !reflect.DeepEqual(topo.FastCliques, [][]int{{2, 4}, {4, 5}}) {
		t.Fatalf("FastCliques = %v", topo.FastCliques)
	}
	if !reflect.DeepEqual(topo.NUMAGroups, map[int][]int{0: {2, 4}, 1: {5}}) {
		t.Fatalf("NUMAGroups = %v", topo.NUMAGroups)
	}
}

// npu-smi on a 910C host numbers the matrix by chip physical id, the same
// number the chips are reported and injected by. SIO joins the two chips of a
// card, HCCS_SW the cards; both are the fast interconnect.
func TestParseAutoAscend910CPhysicalIDs(t *testing.T) {
	raw, err := os.ReadFile("testdata/npu-smi-topo-910c.txt")
	if err != nil {
		t.Fatal(err)
	}
	topo, err := ParseAuto(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.GPUs) != 10 || topo.GPUs[0].Index != 0 || topo.GPUs[9].Index != 9 {
		t.Fatalf("GPUs = %+v", topo.GPUs)
	}
	if topo.Matrix[0][1].Raw != "SIO" || topo.Matrix[0][1].Class != LinkNVLink {
		t.Fatalf("chip 0-1 link = %+v", topo.Matrix[0][1])
	}
	if topo.Matrix[1][2].Raw != "HCCS_SW" || topo.Matrix[1][2].Class != LinkNVLink {
		t.Fatalf("card 0-1 link = %+v", topo.Matrix[1][2])
	}
	if !reflect.DeepEqual(topo.FastCliques, [][]int{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}}) {
		t.Fatalf("FastCliques = %v", topo.FastCliques)
	}
}

func TestParseMatrixErrors(t *testing.T) {
	if _, err := ParseMatrix(""); err == nil {
		t.Error("expected error for empty input")
	}
	if _, err := ParseMatrix("no header here\njust text\n"); err == nil {
		t.Error("expected error for input without header")
	}
}

func TestTopologyJSONCliqueCompatibility(t *testing.T) {
	var old Topology
	if err := json.Unmarshal([]byte(`{"NVLinkCliques":[[0,1]]}`), &old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.FastCliques, [][]int{{0, 1}}) {
		t.Fatalf("old JSON did not backfill FastCliques: %+v", old)
	}

	var next Topology
	if err := json.Unmarshal([]byte(`{"FastCliques":[[2,3]]}`), &next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.NVLinkCliques, [][]int{{2, 3}}) {
		t.Fatalf("new JSON did not backfill NVLinkCliques: %+v", next)
	}
}
