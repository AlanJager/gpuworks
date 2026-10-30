package controller

import (
	"context"
	"errors"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// withdrawDeploymentDownloads releases transfers this service initiated. Another
// active service using the model inherits ownership; independent downloads have
// no service owner and are never considered here.
func (c *Controller) withdrawDeploymentDownloads(ctx context.Context, deploymentID uint) error {
	rows, err := c.store.DownloadsForDeployment(deploymentID)
	if err != nil || len(rows) == 0 {
		return err
	}
	modelIDs := make([]uint, 0, len(rows))
	for _, row := range rows {
		modelIDs = append(modelIDs, row.ModelID)
	}
	deps, err := c.downloadDependents(modelIDs)
	if err != nil {
		return err
	}
	dependencies := make(map[uint][]uint)
	byID := make(map[uint]*store.Deployment)
	for _, dep := range deps {
		if dep.ID == deploymentID || dep.State != store.DeployActive {
			continue
		}
		ids, err := planning.DeploymentModelIDs(&dep)
		if err != nil {
			return err
		}
		dependencies[dep.ID] = ids
		byID[dep.ID] = &dep
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		var successor uint
		for id, ids := range dependencies {
			if slices.Contains(ids, row.ModelID) && (successor == 0 || id < successor) {
				needed, err := c.deploymentNeedsDownload(byID[id], &row)
				if err != nil {
					return err
				}
				if needed {
					successor = id
				}
			}
		}
		if successor != 0 {
			if err := c.store.SetDownloadDeployment(row.ID, row.TransferID, successor, deploymentID); err != nil {
				return err
			}
			continue
		}
		canceled, err := c.store.CancelModelDownload(row.ID, row.TransferID, "Download canceled because the inference service was deleted or stopped", deploymentID)
		if errors.Is(err, store.ErrModelDownloadInactive) {
			continue
		}
		if err != nil {
			return err
		}
		nodeID := row.NodeID
		if nodeID == 0 {
			nodeID = row.WriterNodeID
		}
		if nodeID == 0 {
			continue
		}
		node, err := c.store.NodeByID(nodeID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		model, err := c.store.GetModel(row.ModelID)
		if err != nil {
			return err
		}
		if err := c.pushModelFile(node, canceled, model); err != nil {
			return err
		}
	}
	return nil
}

func refOf(mf *store.ModelFile) store.FileRef {
	return store.FileRef{ModelID: mf.ModelID, StorageID: mf.StorageID, NodeID: mf.NodeID}
}

// A matching model alone does not consume this transfer: the service must
// actually select its storage location on a candidate node.
func (c *Controller) deploymentNeedsDownload(dep *store.Deployment, row *store.ModelFile) (bool, error) {
	rules, err := c.sourceRules(dep)
	if err != nil {
		return false, err
	}
	for _, nodeID := range dep.NodeIDList() {
		if nodeID == 0 {
			continue
		}
		if row.NodeID != 0 && nodeID != row.NodeID {
			continue
		}
		node, err := c.store.NodeByID(nodeID)
		if err != nil {
			return false, err
		}
		source, err := c.store.ModelSourceOn(row.ModelID, node, rules)
		if err != nil {
			return false, err
		}
		if source.Location.File != nil && source.Location.File.ID == row.ID {
			return true, nil
		}
		if source.Location.Shared != nil && source.Location.Shared.ID == row.ID {
			return true, nil
		}
	}
	return false, nil
}
