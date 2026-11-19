package api

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// slurmFixes are the steps that bring a node Slurm will not start jobs on
// back (its slurm_unavailable code), in order, with the commands to run; none
// while Slurm runs jobs there or for a code with no known fix.
func slurmFixes(n *store.Node) []dto.NodeSlurmFix {
	name := n.SlurmNode
	if name == "" {
		return nil
	}
	resume := dto.NodeSlurmFix{Code: "resume", Commands: []string{"scontrol update nodename=" + name + " state=resume"}}
	switch n.SlurmUnavailable {
	case proto.SlurmNodeGRESMissing:
		var fixes []dto.NodeSlurmFix
		if line := slurmGresConf(n); line != "" {
			fixes = append(fixes, dto.NodeSlurmFix{Code: "gres_conf", Commands: []string{line}})
		}
		return append(fixes,
			dto.NodeSlurmFix{Code: "reconfigure", Commands: []string{"scontrol reconfigure"}},
			resume,
			dto.NodeSlurmFix{Code: "restart_slurmd", Commands: []string{"sudo systemctl restart slurmd"}})
	case proto.SlurmNodeDrained:
		resume.Code = "resume_drained"
		return []dto.NodeSlurmFix{resume}
	case proto.SlurmNodeDown, proto.SlurmNodeInvalidRegistration:
		return []dto.NodeSlurmFix{
			{Code: "check_slurmd", Commands: []string{"systemctl status slurmd", "sudo journalctl -u slurmd -n 100 --no-pager"}},
			resume,
		}
	}
	return nil
}

// slurmGresConf is the gres.conf line that lets Slurm see this node's NVIDIA
// cards, for a node whose slurmd registered without them (gres_missing); ""
// for any other state. The cards are named by device index, which is how
// NVIDIA's device files are numbered.
func slurmGresConf(n *store.Node) string {
	if n.SlurmUnavailable != proto.SlurmNodeGRESMissing || n.SlurmNode == "" || nodeDTOGPUVendor(n) != accelerator.VendorNVIDIA || n.GPUCount < 1 {
		return ""
	}
	files := "/dev/nvidia0"
	if n.GPUCount > 1 {
		files = fmt.Sprintf("/dev/nvidia[0-%d]", n.GPUCount-1)
	}
	return fmt.Sprintf("NodeName=%s Name=gpu File=%s", n.SlurmNode, files)
}
