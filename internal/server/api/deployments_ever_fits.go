package api

import (
	"context"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// checkCreateCanEverFit applies checkPoolCanEverFit to a create: only a
// single-node deployment is placed from a pool; a multi-node or PD one names
// its nodes rank by rank and is checked where they are.
func (a *apiHandlers) checkCreateCanEverFit(ctx context.Context, req *dto.DeployRequest, pd bool, reqs map[string][]string, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, modelMount string, componentMounts []proto.MountSpec) error {
	if pd || req.NNodes > 1 {
		return nil
	}
	overrides, _ := requestContainerOverrides(req.ContainerOverrides)
	return a.checkPoolCanEverFit(ctx, req, reqs, engine, model, selection, placement, quick, userExtraArgs, strings.TrimSpace(req.ServedName), modelMount, componentMounts, overrides)
}

// checkPoolCanEverFit refuses a single-node deployment that no node of its
// pool could run even with nothing else on it: too few cards, too little
// memory per card, a dtype the cards do not run, a node that may not serve.
// Such a deployment would wait forever; the request is the moment to say so.
// What only waiting fixes - cards taken, a card unhealthy, a disk full, a node
// offline - never refuses here (planning.Input.Unloaded).
func (a *apiHandlers) checkPoolCanEverFit(ctx context.Context, req *dto.DeployRequest, reqs map[string][]string, engine deploy.Engine, model *store.Model, selection templateSelection, placement planning.Placement, quick deploy.LaunchConfig, userExtraArgs []string, servedName, modelMount string, componentMounts []proto.MountSpec, overrides deploy.ContainerOverrides) error {
	if _, pinned := planning.PinnedNode(reqs); !pinned {
		placement.GPUIndexes = nil // the pool's scheduler picks cards
	}
	previewSelection := snapshotTemplateSelection(selection, req.Image)
	return a.checkEverFits(ctx, reqs, func(node *store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error) {
		return planning.LaunchRequest{
			StartTimeoutSec: req.StartTimeoutSec,
			Engine:          engine.Name(), Model: model, Node: node,
			Selection: previewSelection, Placement: placement, Quick: quick, ExtraArgs: userExtraArgs,
			Topology:   planning.DistTopology{NNodes: 1, Role: "standalone"},
			Source:     planning.LaunchSourceFromRequest(req.Image, overrides),
			ModelMount: modelMount, ComponentMounts: componentMounts,
			ServedName: servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy),
			VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
			APIContract: previewSelection.APIContract,
			// A model larger than the storage's cache cap never fits; the
			// storage's other refusals only wait (planning.Input.Unloaded).
			ModelRules: store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: a.previewSaveToStorageID(req.ModelSource, model, node)},
		}, previewSelection, nil
	})
}

// checkStoredCanEverFit is checkPoolCanEverFit for a stored deployment whose
// requirements are about to become reqs: its replicas launch with the stored
// configuration, exactly as the controller trials a pool candidate
// (planning.DeploymentLaunch).
func (a *apiHandlers) checkStoredCanEverFit(ctx context.Context, dep *store.Deployment, reqs map[string][]string) error {
	model, err := a.d.Store.GetModel(dep.ModelID)
	if err != nil {
		return err
	}
	return a.checkEverFits(ctx, reqs, func(node *store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error) {
		launch, layer, _, err := planning.DeploymentLaunch(dep, reqs, node, model, nil)
		if err != nil {
			return launch, layer, err
		}
		launch.Topology, err = planning.RankTopology(node, nil, planning.GroupRank{Ranks: 1})
		return launch, layer, err
	})
}

// checkEverFits is planning.EverFits over every node, as the product error a
// request is refused with.
func (a *apiHandlers) checkEverFits(ctx context.Context, reqs map[string][]string, build func(*store.Node) (planning.LaunchRequest, deploy.CatalogSelection, error)) error {
	nodes, err := a.candidateNodes(reqs)
	if err != nil {
		return err
	}
	never, err := planning.EverFits(ctx, a.d.Store, a.networks(), nodes, a.resolveForRequirements(nodes, reqs), reqs, build)
	if err != nil || never == nil {
		return err
	}
	params := map[string]any{"pool": never.Pool, "shapes": never.Shapes}
	if never.Node != "" {
		params["node"], params["reason"], params["detail"], params["reason_params"] = never.Node, never.Reason.Code, never.Reason.Message, never.Reason.Params
	}
	return productError(ErrorDeploymentNeverFits, "no node the deployment may run on could fit a replica even with nothing else on it", params)
}

// candidateNodes reads the nodes reqs may admit, which is every node unless
// they name nodes: then no other node can satisfy them, and reading a whole
// fleet per request only to discard all but the named ones made each create
// cost a full node scan.
func (a *apiHandlers) candidateNodes(reqs map[string][]string) ([]*store.Node, error) {
	var rows []store.Node
	var err error
	if named := planning.NamedNodes(reqs); len(named) > 0 {
		rows, err = a.d.Store.NodesByUUID(named)
	} else {
		rows, err = a.d.Store.ListNodes()
	}
	if err != nil {
		return nil, err
	}
	nodes := make([]*store.Node, 0, len(rows))
	for i := range rows {
		nodes = append(nodes, &rows[i])
	}
	return nodes, nil
}
