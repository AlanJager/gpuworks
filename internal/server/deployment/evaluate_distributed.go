package deployment

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// prepareDistributed settles a multi-node draft: its inline preflight, its
// nodes in rank order and the parallelism that takes effect.
func (r *evaluateState) prepareDistributed() bool {
	if r.req.NNodes <= 0 {
		r.req.NNodes = 1
	}
	// A multi-node draft gets the structural preflight inline, computed before
	// the distributed validation can stop the response, so the summary rides
	// along with the field errors it explains. Deep network probing stays with
	// the preflight RPC.
	if r.req.NNodes > 1 {
		r.resp.Preflight = r.s.evaluateInlinePreflight(r.req)
		if !r.resp.Preflight.OK {
			r.resp.OK = false
		}
	}
	if r.req.NNodes > 1 {
		normalized := dto.DeployRequest{
			NodeUUID: r.req.NodeUUID, NodeUUIDs: r.req.NodeUUIDs, NNodes: r.req.NNodes,
			NodeRank: r.req.NodeRank, MasterAddr: r.req.MasterAddr, MasterPort: r.req.MasterPort,
			Placement: r.req.Placement,
		}
		dp, err := deploy.PlacementDataParallel(r.eng.Name(), r.quick.DP, r.launch.TemplateResidual, r.launch.UserResidual)
		if r.errs.add("data_parallel_size", err) {
			r.writeFieldErrors()
			return false
		}
		nodes, err := r.s.PrepareDistributedDeployRequest(&normalized, dp)
		if err != nil {
			field := distributedErrorField(err)
			r.errs.add(field, err)
			if !isParallelismField(field) {
				r.writeFieldErrors()
				return false
			}
			r.parallelInvalid = true
			// The form keeps what the deployer typed: form_values echoes the
			// stated degrees, since a key missing from it clears the field.
			if r.placement.TP > 0 {
				r.resp.FormValues["tp"] = r.placement.TP
			}
			if r.placement.PP > 0 {
				r.resp.FormValues["pp"] = r.placement.PP
			}
			// Rank zero is the node the acquisition facts describe.
			if len(r.req.NodeUUIDs) > 0 {
				r.req.NodeUUID = r.req.NodeUUIDs[0]
			}
		} else {
			r.req.NodeUUID = normalized.NodeUUID
			r.req.NodeRank = normalized.NodeRank
			r.req.MasterAddr = normalized.MasterAddr
			r.req.MasterPort = normalized.MasterPort
			// The parallelism that takes effect, defaulted from the nodes
			// when the draft stated none, is what the form shows and what
			// the plan below runs with.
			r.req.Placement = normalized.Placement
			r.placement = evaluationPlacement(r.req)
			r.resp.FormValues["tp"], r.resp.FormValues["pp"] = r.placement.TP, r.placement.PP
		}
		for _, n := range nodes {
			withGPUs, err := r.s.d.Store.NodeWithGPUs(n.UUID)
			if err != nil {
				r.errs.add("node_uuids", productError(messages.ErrorDeploymentNodeInvalid, fmt.Sprintf("node %s: %v", n.UUID, err), map[string]any{"node": n.UUID, "detail": err.Error()}))
				r.writeFieldErrors()
				return false
			}
			r.rankNodes = append(r.rankNodes, withGPUs)
		}
	}
	return true
}
