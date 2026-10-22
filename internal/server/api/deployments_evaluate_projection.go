package api

import (
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluateWeightSourceOptions projects the selected spec's declared weight
// sources with the two facts the form's chooser needs: whether the server
// suggests a source and whether a matching model row already exists. The
// matching rule is the one the create path already applies
// (templateWeightMatchesModel); this projection only reads it.
func (a *apiHandlers) evaluateWeightSourceOptions(selection templateSelection) []dto.WeightSourceOption {
	sources := specWeightSources(selection.Spec)
	if len(sources) == 0 {
		return nil
	}
	models, err := a.d.Store.ListModels()
	if err != nil {
		models = nil
	}
	out := make([]dto.WeightSourceOption, 0, len(sources))
	for _, source := range sources {
		option := dto.WeightSourceOption{Provider: source.Provider, RepoID: source.RepoID, Revision: source.Revision, Subfolder: source.Subfolder}
		// The suggestion is an identity (provider + repository); its revision
		// may have been resolved further than the declared row spells.
		option.Suggested = selection.SuggestedWeight.Provider == source.Provider && strings.EqualFold(selection.SuggestedWeight.RepoID, source.RepoID)
		for i := range models {
			if templateWeightMatchesModel(source, &models[i]) {
				option.Registered = true
				break
			}
		}
		out = append(out, option)
	}
	return out
}

// evaluateInlinePreflight answers the structural preflight judgements a
// multi-node draft needs on every keystroke. The request-level rules come from
// the preflight RPC's own validator (count, duplicates, whitespace, port
// range) so the two paths can never disagree; each named node's presence,
// online state and routable address are then checked individually. Deep
// network probing - listeners and bandwidth measurements over agent RPCs -
// stays with the preflight RPC, which the CLI keeps calling.
func (a *apiHandlers) evaluateInlinePreflight(req *dto.DeployRequest) *dto.PreflightSummary {
	summary := &dto.PreflightSummary{OK: true}
	fail := func(code string, params map[string]any, message string) {
		summary.OK = false
		summary.Checks = append(summary.Checks, dto.PreflightCheck{Code: code, Params: params, Message: message})
	}
	preflightReq := distributedPreflightRequest{NodeUUIDs: req.NodeUUIDs, MasterAddr: req.MasterAddr}
	if preflightReq.NodeUUIDs == nil && req.NodeUUID != "" {
		preflightReq.NodeUUIDs = []string{req.NodeUUID}
	}
	if req.MasterPort != 0 {
		preflightReq.Port = req.MasterPort
	}
	if err := validatePreflightRequest(&preflightReq); err != nil {
		var productErr *messages.ProductError
		if errors.As(err, &productErr) {
			fail(productErr.Code(), productErr.Params(), productErr.Error())
		} else {
			fail(ErrorFieldInvalid, map[string]any{"detail": err.Error()}, err.Error())
		}
		return summary
	}
	nodes := make([]*store.Node, 0, len(preflightReq.NodeUUIDs))
	for _, uuid := range preflightReq.NodeUUIDs {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if err != nil {
			fail(ErrorPreflightNodeInvalid, map[string]any{"node": uuid}, err.Error())
			continue
		}
		if node.State != store.NodeStateOnline {
			fail(ErrorPreflightNodeOffline, map[string]any{"node": uuid}, "node "+uuid+" is offline")
			continue
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == len(preflightReq.NodeUUIDs) {
		// The ranks must share a network before they can be probed on it.
		if _, err := a.computeGroup(nodes); err != nil {
			var productErr *messages.ProductError
			if errors.As(err, &productErr) {
				fail(productErr.Code(), productErr.Params(), productErr.Error())
			} else {
				fail(ErrorPreflightNodeInvalid, map[string]any{"detail": err.Error()}, err.Error())
			}
		}
	}
	return summary
}
