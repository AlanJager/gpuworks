package api

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ModelPreparation is model preparation where it is produced: the in-memory
// facts (prepobs) — the agents' copy frames in the hub, the nodes the
// controller chose and the cache evictions it runs — and the controller's
// retry of a failed cache copy. Nil shows every unfinished model as waiting
// for an update and offers no retry.
type ModelPreparation interface {
	ModelCopyObservation(file *store.ModelFile) (proto.ModelFileStatus, bool)
	CachePhase(ref store.FileRef) string
	PreparationNodes(dep *store.Deployment) []uint
	// RetryCacheCopy restarts a failed cache copy (controller.RetryCacheCopy).
	RetryCacheCopy(ctx context.Context, ref store.FileRef) error
}

// preparationReads is how both lookups read those observations.
type preparationReads struct{ prep ModelPreparation }

func (r preparationReads) modelCopyObservation(file *store.ModelFile) (proto.ModelFileStatus, bool) {
	if r.prep == nil {
		return proto.ModelFileStatus{}, false
	}
	return r.prep.ModelCopyObservation(file)
}

func (r preparationReads) cachePhase(ref store.FileRef) string {
	if r.prep == nil {
		return ""
	}
	return r.prep.CachePhase(ref)
}

// preparationNodeIDs are the nodes a deployment's weights are being prepared
// on: those its live instances run on plus those the controller chose for
// instances to come, else the nodes it is pinned to.
func (r preparationReads) preparationNodeIDs(dep *store.Deployment, instances []store.Instance) []uint {
	var ids []uint
	if r.prep != nil {
		ids = r.prep.PreparationNodes(dep)
	}
	for _, inst := range instances {
		if inst.State != proto.InstStopped && inst.State != proto.InstFailed {
			ids = append(ids, inst.NodeID)
		}
	}
	if len(ids) == 0 {
		ids = dep.NodeIDList()
	}
	seen := map[uint]bool{}
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id != 0 && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

// deploymentModelProgress is where every model the deployment reads stands on
// every node it is being prepared on. shown includes a model that has no row
// on a node yet, so its waiting reason (an eviction, an offline node) is
// visible; recorded holds only rows that exist, which is what the runtime
// state reads — a row that does not exist says nothing about the deployment.
func deploymentModelProgress(rows dtoLookup, dep *store.Deployment) (shown, recorded []dto.ModelNodePresence) {
	ids, err := planning.DeploymentModelIDs(dep)
	if err != nil {
		return nil, nil
	}
	rules := store.SourceRules{Pinned: store.StoragePins(rows.requirements(dep.ID)), SaveTo: dep.SaveToStorageID}
	for _, nodeID := range rows.preparationNodes(dep) {
		node := rows.node(nodeID)
		if node == nil {
			continue
		}
		for _, modelID := range ids {
			presence, exists := modelPreparationPresence(rows, node, modelID, rules)
			shown = append(shown, presence)
			if exists {
				recorded = append(recorded, presence)
			}
		}
	}
	return shown, recorded
}

// recordedModelProgress is deploymentModelProgress's recorded half.
func recordedModelProgress(rows dtoLookup, dep *store.Deployment) []dto.ModelNodePresence {
	_, recorded := deploymentModelProgress(rows, dep)
	return recorded
}

func modelPreparationPresence(rows dtoLookup, node *store.Node, modelID uint, rules store.SourceRules) (dto.ModelNodePresence, bool) {
	source := rows.modelSource(modelID, node, rules)
	storage := source.Location.Storage
	file := planning.ModelPreparationFile(source)
	exists := file != nil
	if !exists {
		file = &store.ModelFile{ModelID: modelID, NodeID: node.ID, StorageID: storage.ID, State: proto.MFPending}
	}
	presence := modelNodePresence(file, node)
	presence.ModelID = modelID
	if model := rows.model(modelID); model != nil {
		presence.ModelName = model.DisplayName()
	}
	presence.StorageID, presence.StorageName, presence.StorageType = storage.ID, storage.Name, storage.Type
	presence.Cache = storage.Type == proto.StorageNFSCache
	if source.Location.HostPath != "" {
		presence.Path = source.Location.HostPath
	}
	if from := rows.node(file.TransferSourceNodeID); from != nil {
		presence.SourceNodeUUID, presence.SourceNodeName = from.UUID, nodeDisplayName(from)
	}
	var observation *proto.ModelFileStatus
	if observed, ok := rows.modelCopyObservation(file); ok {
		observation = &observed
	}
	view := planning.PrepareModelView(node, source, observation, rows.cachePhase(store.FileRef{ModelID: modelID, NodeID: node.ID, StorageID: storage.ID}))
	presence.Preparation = &dto.ModelPreparation{Phase: view.Phase, Reason: view.Reason, TotalKnown: view.TotalKnown, IdleSeconds: view.IdleSeconds, ETASeconds: view.ETASeconds, Retryable: view.Retryable}
	if view.Observed {
		presence.Downloaded, presence.SizeBytes, presence.TransferredBytes = observation.BytesDone, observation.BytesTotal, observation.TransferBytesDone
		presence.RateBps = observation.RateBytesPerS
		presence.Progress = transferProgress(observation.BytesDone, observation.BytesTotal)
	}
	if !view.RateShown {
		presence.RateBps = 0
	}
	return presence, exists
}

// deploymentPreparationSummary is the list row's one-line preparation: the
// first model still being prepared for an active deployment.
func deploymentPreparationSummary(dep *store.Deployment, progress []dto.ModelNodePresence) *dto.ModelNodePresence {
	if dep.State != store.DeployActive {
		return nil
	}
	for i := range progress {
		if !proto.ModelFileStateReady(progress[i].State) {
			summary := progress[i]
			return &summary
		}
	}
	return nil
}

func (l storeLookup) preparationNodes(dep *store.Deployment) []uint {
	return l.preparationNodeIDs(dep, l.instances(dep.ID))
}
func (l *listLookup) preparationNodes(dep *store.Deployment) []uint {
	return l.preparationNodeIDs(dep, l.instances(dep.ID))
}
func (l storeLookup) modelSource(id uint, node *store.Node, rules store.SourceRules) store.ModelSource {
	source, _ := l.st.ModelSourceOn(id, node, rules)
	return source
}
func (l *listLookup) modelSource(id uint, node *store.Node, rules store.SourceRules) store.ModelSource {
	return l.sources.Source(id, node.ID, rules)
}

func (l *listLookup) loadPreparationSources(deployments []store.Deployment, files []store.ModelFile) error {
	nodeIDs := map[uint]bool{}
	for i := range deployments {
		for _, id := range l.preparationNodes(&deployments[i]) {
			nodeIDs[id] = true
		}
	}
	nodes := make([]*store.Node, 0, len(nodeIDs))
	for id := range nodeIDs {
		if node := l.nodes[id]; node != nil {
			nodes = append(nodes, node)
		}
	}
	var err error
	l.sources, err = l.st.ModelSourcesFrom(nodes, files, l.storageRows)
	return err
}
