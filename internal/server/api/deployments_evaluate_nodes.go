package api

import (
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Eligibility reason codes. They ride the ScheduleReason vocabulary so the
// client translates them the way it translates every other schedule reason;
// they describe the draft's own constraints, so they belong to the deploy form
// rather than the scheduler.
const (
	reasonNodeOffline         = "node_offline"
	reasonNodeCordoned        = "node_cordoned"
	reasonRequirementsUnmet   = "node_requirements_unmet"
	reasonRequirementsInvalid = "node_requirements_invalid"
	reasonVendorMismatch      = "vendor_mismatch"
	reasonChipMismatch        = "chip_mismatch"
)

// evaluateEligibleNodes runs every node through the predicate chain the draft
// itself states - online state, whether it takes new replicas, the node requirements (vendor included) and
// the template's GPU predicate - and reports each node eligible or not with
// its reasons. It is O(nodes) over the same facts evaluate already resolves
// and deliberately does no scheduling: scoring is the candidates list's job,
// not this one. Requirements that could not be read (reqsErr) make every
// node ineligible: the dropdown must not offer nodes the draft may exclude.
//
// For an automatic draft (automatic) it also answers which dGPU backend the
// eligible pool offers: the first eligible node's ready one
// (partitioncap.Preferred), nil when none has one. Such a draft has no node to
// read that from until a placement fits, and a placement that only fits as a
// dGPU slice never fits before the quota is set, so the form gates its quota
// on this instead. A draft that names its node reads the node's own, so the
// per-node derivation is skipped for it.
func (a *apiHandlers) evaluateEligibleNodes(selection templateSelection, reqs map[string][]string, reqsErr error, automatic bool) ([]dto.EligibleNode, *proto.PartitionCapability) {
	nodes, err := a.d.Store.ListNodes()
	if err != nil || len(nodes) == 0 {
		return nil, nil
	}
	var predicate *chipmatch.Predicate
	var unsupported []chipmatch.Unsupported
	if selection.Spec != nil {
		decoded, err := decodeSpecPredicate(*selection.Spec)
		if err != nil {
			predicate = nil
		} else {
			predicate = &decoded
		}
		if selection.Model != nil {
			unsupported, _ = decodeUnsupported(*selection.Model)
		}
	}
	all := make([]*store.Node, 0, len(nodes))
	for index := range nodes {
		all = append(all, &nodes[index])
	}
	resolved := a.resolveForRequirements(all, reqs)
	out := make([]dto.EligibleNode, 0, len(nodes))
	var poolPartition *proto.PartitionCapability
	for _, node := range all {
		name := node.Name
		if name == "" {
			name = node.Hostname
		}
		entry := dto.EligibleNode{UUID: node.UUID, Name: name, Eligible: true}
		reason := func(code string, params map[string]any) {
			entry.Eligible = false
			entry.Reasons = append(entry.Reasons, dto.ScheduleReason{Code: code, Params: params})
		}
		if node.State != store.NodeStateOnline {
			reason(reasonNodeOffline, nil)
		}
		if !node.Scheduling.TakesReplicas() {
			reason(reasonNodeCordoned, map[string]any{"node": node.DisplayName()})
		}
		if reqsErr != nil {
			reason(reasonRequirementsInvalid, map[string]any{"detail": reqsErr.Error()})
		} else if err := planning.MatchRequirements(node, resolved, reqs); err != nil {
			var unmet *planning.UnmetRequirement
			switch {
			case errors.As(err, &unmet) && planning.IsVendorRequirement(unmet):
				reason(reasonVendorMismatch, map[string]any{"vendor": planning.RequirementVendor(reqs)})
			default:
				reason(reasonRequirementsUnmet, map[string]any{"detail": err.Error()})
			}
		}
		if predicate != nil && !nodeMatchesChipPredicate(node, *predicate, unsupported) {
			reason(reasonChipMismatch, map[string]any{"predicate": strings.TrimSpace(selection.Spec.GPUs)})
		}
		if automatic && entry.Eligible && poolPartition == nil {
			if capability, ok := partitioncap.Preferred(node); ok {
				poolPartition = &capability
			}
		}
		out = append(out, entry)
	}
	return out, poolPartition
}

// nodeMatchesChipPredicate answers whether any reported GPU satisfies the
// template's GPU predicate under the model's unsupported rules.
func nodeMatchesChipPredicate(node *store.Node, predicate chipmatch.Predicate, unsupported []chipmatch.Unsupported) bool {
	for _, gpu := range node.OwnedGPUs() {
		reported := chipmatch.ReportedGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.ComputeCapability)
		selection := chipmatch.Select(reported, []chipmatch.Spec{{GPUs: predicate}}, unsupported)
		if selection.Matched {
			return true
		}
	}
	return false
}
