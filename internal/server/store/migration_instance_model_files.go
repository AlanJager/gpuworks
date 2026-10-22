package store

import (
	"encoding/json"
	"path"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// modelComponentPurpose prefixes the purpose of a template component's mount.
const modelComponentPurpose = "model_component:"

// migrateInstanceModelFiles creates instance_model_files and records, for
// every instance that still holds its files, the model files its desired state
// mounts: the primary model path and each template component. A mount is
// matched to the row whose host path it is on the instance's node; a mount no
// row explains (a file since removed) is left unrecorded. Re-running skips an
// instance that already has rows.
func migrateInstanceModelFiles(db *gorm.DB) error {
	if err := db.AutoMigrate(&InstanceModelFile{}); err != nil {
		return err
	}
	for _, table := range []any{&Instance{}, &ModelFile{}, &StorageNodeState{}} {
		if !db.Migrator().HasTable(table) {
			return nil // a schema being built from scratch has no instances yet
		}
	}
	var insts []Instance
	if err := db.Where("state NOT IN ?", endedInstanceStates).Order("id asc").Find(&insts).Error; err != nil {
		return err
	}
	byNode := map[uint]map[string]ModelFile{}
	for _, inst := range insts {
		var recorded int64
		if err := db.Model(&InstanceModelFile{}).Where("instance_id = ?", inst.ID).Count(&recorded).Error; err != nil {
			return err
		}
		if recorded > 0 {
			continue
		}
		var spec proto.InstanceDesiredState
		if json.Unmarshal([]byte(inst.DesiredStateJSON), &spec) != nil {
			continue
		}
		files, ok := byNode[inst.NodeID]
		if !ok {
			var err error
			if files, err = hostPathsOnNode(db, inst.NodeID); err != nil {
				return err
			}
			byNode[inst.NodeID] = files
		}
		for _, mounted := range mountedModelPaths(spec) {
			row, ok := files[path.Clean(mounted)]
			if !ok {
				continue
			}
			if err := db.Create(&InstanceModelFile{InstanceID: inst.ID, ModelID: row.ModelID, ModelFileID: row.ID, HostPath: mounted}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// mountedModelPaths are the host paths of an instance's model mounts.
func mountedModelPaths(spec proto.InstanceDesiredState) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(spec.ModelPath)
	for _, mount := range spec.Mounts {
		if strings.HasPrefix(mount.Purpose, modelComponentPurpose) {
			add(mount.Source)
		}
	}
	return out
}

// hostPathsOnNode maps each host path a node reads model files at to its
// row: the node's own rows (local files and cache entries) by their path, and
// shared storages' rows under the node's mount point.
func hostPathsOnNode(db *gorm.DB, nodeID uint) (map[string]ModelFile, error) {
	out := map[string]ModelFile{}
	var own []ModelFile
	if err := db.Where("node_id = ?", nodeID).Find(&own).Error; err != nil {
		return nil, err
	}
	for _, row := range own {
		if strings.TrimSpace(row.Path) != "" {
			out[path.Clean(row.Path)] = row
		}
	}
	var states []StorageNodeState
	if err := db.Where("node_id = ? AND path <> ''", nodeID).Find(&states).Error; err != nil {
		return nil, err
	}
	for _, state := range states {
		var shared []ModelFile
		if err := db.Where("storage_id = ? AND node_id = 0", state.StorageID).Find(&shared).Error; err != nil {
			return nil, err
		}
		for _, row := range shared {
			if host, ok := SharedHostPath(state.Path, row.Path); ok {
				out[host] = row
			}
		}
	}
	return out, nil
}
