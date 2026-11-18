package api

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

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
