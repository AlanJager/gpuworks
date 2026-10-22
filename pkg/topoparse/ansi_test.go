package topoparse

import "testing"

func TestParseMatrixANSIAndNumaCols(t *testing.T) {
	// Real `nvidia-smi topo -m` output from a 2-GPU box: ANSI-underlined header,
	// NODE interconnect, CPU Affinity / NUMA Affinity / GPU NUMA ID columns.
	out := "\t\x1b[4mGPU0\tGPU1\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID\x1b[0m\n" +
		"GPU0\t X \tNODE\t0-19\t0\t\tN/A\n" +
		"GPU1\tNODE\t X \t0-19\t0\t\tN/A\n" +
		"\nLegend:\n  X = Self\n"
	topo, err := ParseMatrix(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.GPUs) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(topo.GPUs))
	}
	if len(topo.Matrix) != 2 || len(topo.Matrix[0]) != 2 {
		t.Fatalf("expected 2x2 matrix, got %dx?", len(topo.Matrix))
	}
	if topo.GPUs[0].NUMANode != 0 || topo.GPUs[1].NUMANode != 0 {
		t.Errorf("expected both GPUs on NUMA 0, got %d,%d", topo.GPUs[0].NUMANode, topo.GPUs[1].NUMANode)
	}
	if len(topo.NUMAGroups[0]) != 2 {
		t.Errorf("expected NUMA 0 group of 2, got %v", topo.NUMAGroups)
	}
	// NODE is not NVLink → no cliques.
	if len(topo.NVLinkCliques) != 0 {
		t.Errorf("expected no NVLink cliques, got %v", topo.NVLinkCliques)
	}
}
