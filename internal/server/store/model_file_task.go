package store

import (
	"errors"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ModelFileTask is the single producer of the task an agent executes for one
// row; the full desired state and the incremental push both use it, so the
// two can never disagree. ok is false when the row is nothing for the node to
// do (a complete shared row, or one whose copy source cannot be resolved).
func (s *Store) ModelFileTask(mf *ModelFile, model *Model) (proto.ModelFileTask, bool) {
	return s.modelFileTask(mf, model, &taskContext{store: s})
}

// taskContext is what building tasks reads besides the row and its model:
// the storages and this control plane's identity, read once for a whole
// node's tasks rather than per row.
type taskContext struct {
	store      *Store
	storages   map[uint]*Storage
	identityID string
}

func (c *taskContext) storage(id uint) (*Storage, error) {
	if c.storages == nil {
		c.storages = map[uint]*Storage{}
	}
	if storage, ok := c.storages[id]; ok {
		if storage == nil {
			return nil, ErrNotFound
		}
		return storage, nil
	}
	storage, err := c.store.StorageByID(id)
	if errors.Is(err, ErrNotFound) {
		c.storages[id] = nil
	} else if err == nil {
		c.storages[id] = storage
	}
	return storage, err
}

func (c *taskContext) controlPlaneID() string {
	if c.identityID == "" {
		c.identityID = c.store.ControlPlaneIdentity().ID
	}
	return c.identityID
}

func (s *Store) modelFileTask(mf *ModelFile, model *Model, ctx *taskContext) (proto.ModelFileTask, bool) {
	if mf == nil || model == nil {
		return proto.ModelFileTask{}, false
	}
	task := proto.ModelFileTask{
		Key:          ModelFileKey(mf.ID),
		ModelID:      mf.ModelID,
		Source:       model.Source,
		SourceID:     model.SourceID,
		Revision:     model.Revision,
		Subfolder:    model.ArtifactSubfolder,
		TargetPath:   mf.Path, // for agents that predate Path/RelPath
		DesiredState: "present",
		RateLimitKBs: mf.RateLimit,
		Acquisition:  mf.Acquisition,
		TransferID:   mf.TransferID,
		StorageID:    mf.StorageID,
	}
	if mf.State == proto.MFCanceled {
		task.DesiredState = "paused"
	}
	// Where a download is fetched from is planned from the pull source rules
	// as they are when the task is sent (planning.PlanModelFetch). An HTTP
	// directory's address is the model's own (SourceURL, set for that source
	// only), so it travels with the task.
	if strings.TrimSpace(model.SourceURL) != "" {
		task.Endpoint = strings.TrimSpace(model.SourceURL)
	}
	task.Path, task.RelPath = splitTaskPath(mf.Path)
	storage, err := ctx.storage(mf.StorageID)
	if err != nil {
		return task, mf.StorageID == 0
	}
	if mf.State == proto.MFDeleting {
		task.DesiredState = "absent"
		return task, true
	}
	shared := proto.StorageShared(storage.Type)
	if shared && mf.NodeID == 0 {
		if mf.Ready() || mf.State == proto.MFMissing || mf.WriterNodeID == 0 {
			return proto.ModelFileTask{}, false
		}
		task.Staging = proto.StagingRelPath(ctx.controlPlaneID(), mf.Path)
	}
	if storage.Type == proto.StorageNFSCache && mf.NodeID != 0 {
		task.Cache = true
		if mf.Acquisition == proto.ModelAcquireCopy {
			switch mf.State {
			case proto.MFAbsent, proto.MFFailed, proto.MFInsufficientDisk:
				// Reconnect must not silently retry a failed cache intent.
				// Withdrawal does not depend on the source still existing.
				task.DesiredState = "absent"
				return task, true
			}
		}
	}
	if mf.Acquisition == proto.ModelAcquireCopy {
		source, err := s.copySourcePath(mf, storage)
		if err != nil {
			return proto.ModelFileTask{}, false
		}
		task.SourcePath = source
	}
	return task, true
}

// splitTaskPath names which kind of path a row holds, so the agent does not
// have to guess. A row's path is an absolute host directory once the node has
// reported where it wrote the model (a local storage's or a node cache's own
// row), and relative to its storage's root otherwise: always for a shared
// storage's row, and for a row that has not been written yet.
func splitTaskPath(path string) (abs, rel string) {
	if filepath.IsAbs(path) {
		return path, ""
	}
	return "", path
}

// copySourcePath resolves the directory a same-node copy reads from. A node
// cache copies from the storage's own shared row; any other copy reads the
// recorded source storage on the node that executes it.
func (s *Store) copySourcePath(mf *ModelFile, storage *Storage) (string, error) {
	executor := mf.NodeID
	if executor == 0 {
		executor = mf.WriterNodeID
	}
	if executor == 0 {
		return "", errors.New("copy has no executing node")
	}
	sourceStorage := mf.TransferSourceStorageID
	if storage.Type == proto.StorageNFSCache && mf.NodeID != 0 {
		sourceStorage = storage.ID
	}
	if sourceStorage == 0 {
		return "", errors.New("copy has no source storage")
	}
	src, err := s.StorageByID(sourceStorage)
	if err != nil {
		return "", err
	}
	if proto.StorageShared(src.Type) {
		shared, err := s.ModelFileAt(FileRef{ModelID: mf.ModelID, StorageID: src.ID})
		if err != nil {
			return "", err
		}
		state, err := s.StorageNodeStateFor(src.ID, executor)
		if err != nil {
			return "", err
		}
		host, ok := SharedHostPath(state.Path, shared.Path)
		if !ok {
			return "", errors.New("source storage is not mounted on the executing node")
		}
		return host, nil
	}
	local, err := s.ModelFileAt(FileRef{ModelID: mf.ModelID, StorageID: src.ID, NodeID: executor})
	if err != nil {
		return "", err
	}
	if !local.Ready() {
		return "", errors.New("source model file is not complete")
	}
	return local.Path, nil
}

// ModelFileTasksForNode lists every task a node executes: its own rows and the
// shared rows it was picked to write.
func (s *Store) ModelFileTasksForNode(nodeID uint) ([]proto.ModelFileTask, error) {
	var mfs []ModelFile
	if err := s.db.Where("node_id = ? OR (node_id = 0 AND writer_node_id = ?)", nodeID, nodeID).Order("id asc").Find(&mfs).Error; err != nil {
		return nil, err
	}
	models := map[uint]*Model{}
	ctx := &taskContext{store: s}
	tasks := make([]proto.ModelFileTask, 0, len(mfs))
	for i := range mfs {
		mf := &mfs[i]
		model := models[mf.ModelID]
		if model == nil {
			m, err := s.GetModel(mf.ModelID)
			if err != nil {
				continue
			}
			model = m
			models[mf.ModelID] = m
		}
		if task, ok := s.modelFileTask(mf, model, ctx); ok {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}
