package deployment

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
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
		recommend := r.excludeDeployment == 0 && needsDistributedRecommendation(r.req, r.resp.AutoPlaced)
		normalized := cloneRequest(*r.req)
		dp, err := deploy.PlacementDataParallel(r.eng.Name(), r.quick.DP, r.launch.TemplateResidual, r.launch.UserResidual)
		if r.errs.add("data_parallel_size", err) {
			r.writeFieldErrors()
			return false
		}
		var nodes []*store.Node
		// An invalid rank command still needs the engine draft that corrects
		// it. The command error is attached once those ranks are previewed.
		if recommend && checkManualCommandRanks(r.req) == nil {
			nodes, err = r.s.recommendDistributed(r.ctx, &normalized, dp, r.eng, r.launch, r.selection, r.model)
		} else {
			nodes, err = r.s.PrepareDistributedDeployRequest(&normalized, dp)
		}
		if err != nil {
			field := distributedErrorField(err, r.placement)
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
			// Keep suggested degrees in the summary. Echoing them as form inputs
			// would pin them when a model or node changes.
			r.req.Placement = normalized.Placement
			if !recommend || r.placement.TP > 0 {
				r.resp.FormValues["tp"] = normalized.Placement["tp"]
			}
			if !recommend || r.placement.PP > 0 {
				r.resp.FormValues["pp"] = normalized.Placement["pp"]
			}
			r.placement = evaluationPlacement(r.req)
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
