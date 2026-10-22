package store

import (
	"errors"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Reasons a model is not ready at a location.
const (
	LocationNotOnNode            = "not_on_node"              // local storage: this node has no complete copy
	LocationStorageNotOnNode     = "storage_not_on_node"      // the storage does not apply to the node
	LocationStorageUnavailable   = "storage_unavailable"      // the storage is not ready on the node
	LocationModelMissingInStore  = "model_missing_in_storage" // a shared storage has no complete copy
	LocationNotCached            = "not_cached"               // nfs_cache: the node has not cached it yet
	LocationStorageNotFound      = "storage_not_found"
	LocationStorageDisabled      = "storage_disabled" // a local storage that would have to take the model is read-only
	locationUnsupportedStorage   = "unsupported_storage"
	locationSharedRowPathInvalid = "shared_path_invalid"
)

// ModelLocation is where a model's files are readable on one node from one
// storage, and the host path that is. ModelSourceOn asks it for each storage
// that applies to a node and picks among the answers; that pick, not any one
// location, is what a launch mounts. The shared host path is composed only
// here (SharedHostPath).
type ModelLocation struct {
	Storage Storage
	// File is the row this node reads: the local row, the shared row, or the
	// node's cache row. Nil when there is none yet.
	File *ModelFile
	// Shared is a shared storage's single row (nil for local storages).
	Shared    *ModelFile
	HostPath  string
	SizeBytes int64
	Ready     bool
	Reason    string
	// NodeState is the node's state for the storage, when it reported one.
	NodeState *StorageNodeState
}

// Ready reports whether the row holds the complete model at a known path. A
// nil row is not ready.
func (mf *ModelFile) Ready() bool {
	return mf != nil && proto.ModelFileStateReady(mf.State) && strings.TrimSpace(mf.Path) != ""
}

// ModelLocation reports whether a model is readable on a node from one storage
// and at which host path. A storage id of 0 is the default local storage.
func (s *Store) ModelLocation(modelID, storageID, nodeID uint) (ModelLocation, error) {
	if storageID == 0 {
		storageID = s.DefaultLocalStorageID()
	}
	storage, err := s.StorageByID(storageID)
	if errors.Is(err, ErrNotFound) {
		return ModelLocation{Reason: LocationStorageNotFound}, nil
	}
	if err != nil {
		return ModelLocation{}, err
	}
	node, err := s.NodeByID(nodeID)
	if err != nil {
		return ModelLocation{}, err
	}
	return s.modelLocationOn(modelID, storage, node)
}

func (s *Store) modelLocationOn(modelID uint, storage *Storage, node *Node) (ModelLocation, error) {
	facts, err := s.loadNodeModelFacts(modelID, node)
	if err != nil {
		return ModelLocation{Storage: *storage}, err
	}
	if !facts.applies(storage.ID) {
		return ModelLocation{Storage: *storage, Reason: LocationStorageNotOnNode}, nil
	}
	return facts.locate(storage), nil
}

// nodeModelFacts is everything locating one model on one node reads, loaded
// in a fixed number of queries whatever the number of storages: the storages
// that apply to the node, the node's state for each, and the model's rows in
// them - the shared copies (node 0) and the node's own.
type nodeModelFacts struct {
	storages []EffectiveStorage
	states   map[uint]*StorageNodeState
	shared   map[uint]*ModelFile
	own      map[uint]*ModelFile
}

func (s *Store) loadNodeModelFacts(modelID uint, node *Node) (nodeModelFacts, error) {
	facts := nodeModelFacts{states: map[uint]*StorageNodeState{}, shared: map[uint]*ModelFile{}, own: map[uint]*ModelFile{}}
	var err error
	if facts.storages, err = s.StoragesForNode(node); err != nil {
		return facts, err
	}
	var states []StorageNodeState
	if err := s.reader().Where("node_id = ?", node.ID).Find(&states).Error; err != nil {
		return facts, err
	}
	for i := range states {
		facts.states[states[i].StorageID] = &states[i]
	}
	var files []ModelFile
	if err := s.reader().Where("model_id = ? AND node_id IN ?", modelID, []uint{0, node.ID}).Find(&files).Error; err != nil {
		return facts, err
	}
	for i := range files {
		if files[i].NodeID == 0 {
			facts.shared[files[i].StorageID] = &files[i]
		} else {
			facts.own[files[i].StorageID] = &files[i]
		}
	}
	return facts, nil
}

func (f nodeModelFacts) applies(storageID uint) bool {
	for _, eff := range f.storages {
		if eff.Storage.ID == storageID {
			return true
		}
	}
	return false
}

// locate is where the model is readable from one storage that applies to the
// node, and at which host path, or why it is not.
func (f nodeModelFacts) locate(storage *Storage) ModelLocation {
	loc := ModelLocation{Storage: *storage, NodeState: f.states[storage.ID]}
	switch storage.Type {
	case proto.StorageLocal:
		mf := f.own[storage.ID]
		loc.File = mf
		if !mf.Ready() {
			loc.Reason = LocationNotOnNode
			return loc
		}
		loc.HostPath, loc.SizeBytes, loc.Ready = mf.Path, mf.SizeBytes, true
		return loc
	case proto.StorageNFS, proto.StorageSharedDir, proto.StorageNFSCache:
		shared := f.shared[storage.ID]
		loc.Shared = shared
		if shared != nil {
			loc.SizeBytes = shared.SizeBytes
		}
		if storage.Type == proto.StorageNFSCache {
			// The node's cache comes first: a cached copy stays readable when
			// the authoritative one is missing (it is then the last copy).
			cache := f.own[storage.ID]
			loc.File = cache
			if cache.Ready() {
				loc.HostPath, loc.Ready = cache.Path, true
				return loc
			}
			switch {
			case !shared.Ready():
				loc.Reason = LocationModelMissingInStore
			case loc.NodeState == nil || loc.NodeState.State != proto.StorageStateReady:
				loc.Reason = LocationStorageUnavailable
			default:
				loc.Reason = LocationNotCached
			}
			return loc
		}
		if !shared.Ready() {
			loc.Reason = LocationModelMissingInStore
			return loc
		}
		loc.File = shared
		if loc.NodeState == nil || loc.NodeState.State != proto.StorageStateReady || loc.NodeState.Path == "" {
			loc.Reason = LocationStorageUnavailable
			return loc
		}
		host, ok := SharedHostPath(loc.NodeState.Path, shared.Path)
		if !ok {
			loc.Reason = locationSharedRowPathInvalid
			return loc
		}
		loc.HostPath, loc.Ready = host, true
		return loc
	default:
		loc.Reason = locationUnsupportedStorage
		return loc
	}
}

// SharedHostPath joins a node's mount point and a storage-relative path. It is
// the only place that composition happens.
func SharedHostPath(mountPoint, rel string) (string, bool) {
	clean, err := storageRelPathClean(rel)
	if err != nil || strings.TrimSpace(mountPoint) == "" {
		return "", false
	}
	return path.Join(mountPoint, clean), true
}

// ModelLocations lists a model's location in every storage that applies to a
// node, local storages first. Evaluation and template resolution read it to
// show what is already there.
func (s *Store) ModelLocations(modelID, nodeID uint) ([]ModelLocation, error) {
	node, err := s.NodeByID(nodeID)
	if err != nil {
		return nil, err
	}
	facts, err := s.loadNodeModelFacts(modelID, node)
	if err != nil {
		return nil, err
	}
	var local, shared []ModelLocation
	for _, eff := range facts.storages {
		storage := eff.Storage
		loc := facts.locate(&storage)
		if storage.Type == proto.StorageLocal {
			local = append(local, loc)
		} else {
			shared = append(shared, loc)
		}
	}
	return append(local, shared...), nil
}

// ReadyLocation is the copy of a model a node reads now, if it has one: the
// one store.ModelSourceOn picks for a deployment with no storage rules.
func (s *Store) ReadyLocation(modelID, nodeID uint) (*ModelLocation, error) {
	node, err := s.NodeByID(nodeID)
	if err != nil {
		return nil, err
	}
	src, err := s.ModelSourceOn(modelID, node, SourceRules{})
	if err != nil || src.Action != SourceReady {
		return nil, err
	}
	return &src.Location, nil
}
