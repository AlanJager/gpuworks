package api

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Each code Slurm gives for not running jobs on a node comes with the steps
// that bring it back, in order, and the commands for this node; a missing
// gres.conf line is offered only when the server can compose it (NVIDIA
// cards it knows the count of). Nothing while Slurm runs jobs, outside
// Slurm, or for a code with no known fix.
func TestSlurmFixesForEachCode(t *testing.T) {
	resume := dto.NodeSlurmFix{Code: "resume", Commands: []string{"scontrol update nodename=gpu7 state=resume"}}
	tail := []dto.NodeSlurmFix{
		{Code: "reconfigure", Commands: []string{"scontrol reconfigure"}},
		resume,
		{Code: "restart_slurmd", Commands: []string{"sudo systemctl restart slurmd"}},
	}
	check := []dto.NodeSlurmFix{
		{Code: "check_slurmd", Commands: []string{"systemctl status slurmd", "sudo journalctl -u slurmd -n 100 --no-pager"}},
		resume,
	}
	cases := []struct {
		name string
		node store.Node
		want []dto.NodeSlurmFix
	}{
		{"gres missing, 2 NVIDIA cards", store.Node{SlurmNode: "gpu7", SlurmUnavailable: proto.SlurmNodeGRESMissing, GPUVendor: accelerator.VendorNVIDIA, GPUCount: 2},
			append([]dto.NodeSlurmFix{{Code: "gres_conf", Commands: []string{"NodeName=gpu7 Name=gpu File=/dev/nvidia[0-1]"}}}, tail...)},
		{"gres missing, card count unknown", store.Node{SlurmNode: "gpu7", SlurmUnavailable: proto.SlurmNodeGRESMissing, GPUVendor: accelerator.VendorNVIDIA}, tail},
		{"drained", store.Node{SlurmNode: "gpu7", SlurmUnavailable: proto.SlurmNodeDrained},
			[]dto.NodeSlurmFix{{Code: "resume_drained", Commands: resume.Commands}}},
		{"down", store.Node{SlurmNode: "gpu7", SlurmUnavailable: proto.SlurmNodeDown}, check},
		{"registration refused", store.Node{SlurmNode: "gpu7", SlurmUnavailable: proto.SlurmNodeInvalidRegistration}, check},
		{"running jobs", store.Node{SlurmNode: "gpu7"}, nil},
		{"unknown code", store.Node{SlurmNode: "gpu7", SlurmUnavailable: "something_new"}, nil},
		{"outside Slurm", store.Node{SlurmUnavailable: proto.SlurmNodeDrained}, nil},
	}
	for _, tc := range cases {
		if got := slurmFixes(&tc.node); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}
