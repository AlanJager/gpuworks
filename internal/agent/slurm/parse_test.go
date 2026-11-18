package slurm

import (
	"slices"
	"testing"
)

func TestGPUIDX(t *testing.T) {
	cases := map[string][]int{
		"gpu:t4:1(IDX:0)":                    {0},
		"gpu:2(IDX:0-1)":                     {0, 1},
		"gpu:a100:3(IDX:0,2-3),nic:1(IDX:0)": {0, 2, 3},
		"gpu:t4:0(IDX:N/A)":                  nil,
		"":                                   nil,
	}
	for in, want := range cases {
		got, err := gpuIDX(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("gpuIDX(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := gpuIDX("gpu:1(IDX:x)"); err == nil {
		t.Error("gpuIDX accepted a malformed index")
	}
}

func TestHostlistHas(t *testing.T) {
	cases := []struct {
		list, node string
		want       bool
	}{
		{"n1", "n1", true},
		{"n1", "n10", false},
		{"n[1-3,7]", "n2", true},
		{"n[1-3,7]", "n7", true},
		{"n[1-3,7]", "n5", false},
		{"gpu[01-02]", "gpu02", true},
		{"gpu[01-02]", "gpu2", false},
		{"a,gpu-b[1-2]", "gpu-b2", true},
		{"slinky-[0-1]", "slinky-0", true},
	}
	for _, c := range cases {
		if got := hostlistHas(c.list, c.node); got != c.want {
			t.Errorf("hostlistHas(%q, %q) = %v; want %v", c.list, c.node, got, c.want)
		}
	}
}

func TestNodeGRESPicksTheNodesGroup(t *testing.T) {
	line := "JobId=7 JobState=RUNNING NodeList=n[1-2] JOB_GRES=gpu:4 Nodes=n1 CPU_IDs=0-1 Mem=10 GRES=gpu:2(IDX:0-1) Nodes=n2 CPU_IDs=0 Mem=10 GRES=gpu:2(IDX:2-3) MinCPUsNode=1"
	if got := nodeGRES(line, "n2"); got != "gpu:2(IDX:2-3)" {
		t.Fatalf("nodeGRES = %q", got)
	}
	if got := nodeGRES(line, "n3"); got != "" {
		t.Fatalf("nodeGRES for a node the job is not on = %q", got)
	}
}

func TestDeviceOrderExpandsRanges(t *testing.T) {
	out := []byte("slurmd: Gres Name=gpu Type=a100 Count=2 Index=0 ID=1 File=/dev/nvidia[2-3] Links=(null)\nslurmd: Gres Name=gpu Type=a100 Count=1 Index=0 ID=1 File=/dev/nvidia0\n")
	if got := deviceOrder(out); !slices.Equal(got, []int{2, 3, 0}) {
		t.Fatalf("deviceOrder = %v", got)
	}
	if got := deviceOrder([]byte("slurmd: Gres Name=gpu Count=1 File=/dev/dri/renderD128\n")); got != nil {
		t.Fatalf("deviceOrder of non-NVIDIA devices = %v; want nil", got)
	}
}
