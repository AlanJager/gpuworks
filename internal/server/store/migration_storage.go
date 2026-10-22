package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// defaultStorageName is the name of the local storage a fresh install starts
// with and the one the former built-in row is renamed to.
const defaultStorageName = "default"

// legacyBuiltinStorageName is the seed name the former built-in row carried.
const legacyBuiltinStorageName = "Local Storage"

// migrateStorageFirstClass is migration 122. The former built-in row becomes an
// ordinary default local storage that applies to every node and uses the
// agent's default directory; each node's models_root becomes that storage's
// override for the node, so no node's directory changes. A database without
// any storage row gets the default local storage: this is the only place a
// storage is created on the operator's behalf.
func migrateStorageFirstClass(db *gorm.DB) error {
	if err := db.AutoMigrate(&Storage{}, &StorageNode{}, &StorageNodeState{}); err != nil {
		return err
	}
	hasBuiltin, err := tableHasColumn(db, "storages", "builtin")
	if err != nil {
		return err
	}
	if hasBuiltin {
		if err := db.Exec(`UPDATE storages SET type = ?, is_default = 1, all_nodes = 1, path = '' WHERE builtin = 1`, proto.StorageLocal).Error; err != nil {
			return err
		}
		var clash int64
		if err := db.Model(&Storage{}).Where("name = ?", defaultStorageName).Count(&clash).Error; err != nil {
			return err
		}
		if clash == 0 {
			if err := db.Exec(`UPDATE storages SET name = ? WHERE builtin = 1 AND name = ?`, defaultStorageName, legacyBuiltinStorageName).Error; err != nil {
				return err
			}
		}
	}
	var count int64
	if err := db.Model(&Storage{}).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		if err := db.Create(&Storage{Name: defaultStorageName, Type: proto.StorageLocal, Enabled: true, IsDefault: true, AllNodes: true}).Error; err != nil {
			return err
		}
	}
	defaultID, err := migrationDefaultLocalStorageID(db)
	if err != nil || defaultID == 0 {
		return err
	}
	hasModelsRoot, err := tableHasColumn(db, "nodes", "models_root")
	if err != nil || !hasModelsRoot {
		return err
	}
	var roots []struct {
		ID         uint
		ModelsRoot string
	}
	if err := db.Raw(`SELECT id, models_root FROM nodes WHERE models_root IS NOT NULL AND trim(models_root) <> ''`).Scan(&roots).Error; err != nil {
		return err
	}
	for _, root := range roots {
		var existing int64
		if err := db.Model(&StorageNode{}).Where("storage_id = ? AND node_id = ?", defaultID, root.ID).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			continue
		}
		if err := db.Create(&StorageNode{StorageID: defaultID, NodeID: root.ID, Path: strings.TrimSpace(root.ModelsRoot)}).Error; err != nil {
			return err
		}
	}
	return nil
}

// migrationDefaultLocalStorageID is the storage existing node-local rows and
// deployments belonged to before storages existed: the local storage that uses
// the agent's default directory, the default one first.
func migrationDefaultLocalStorageID(db *gorm.DB) (uint, error) {
	var ids []uint
	err := db.Model(&Storage{}).Where("type = ?", proto.StorageLocal).
		Order("CASE WHEN path = '' OR path IS NULL THEN 0 ELSE 1 END, is_default DESC, id ASC").
		Limit(1).Pluck("id", &ids).Error
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	return ids[0], nil
}

// migrateModelFileStorage is migration 123: model files are keyed by
// (model, storage, node), existing rows belong to the default local storage,
// and a shared storage holds at most one model directory per path.
func migrateModelFileStorage(db *gorm.DB) error {
	if !db.Migrator().HasTable("model_files") {
		return nil
	}
	if err := db.Exec(`DROP INDEX IF EXISTS idx_mf`).Error; err != nil {
		return err
	}
	if err := db.AutoMigrate(&ModelFile{}); err != nil {
		return err
	}
	defaultID, err := migrationDefaultLocalStorageID(db)
	if err != nil {
		return err
	}
	if defaultID != 0 {
		if err := db.Exec(`UPDATE model_files SET storage_id = ? WHERE storage_id IS NULL OR storage_id = 0`, defaultID).Error; err != nil {
			return err
		}
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_mf_shared_path ON model_files(storage_id, path) WHERE node_id = 0`).Error
}

// migrateDeploymentWeightStorage is migration 124. It added the column a
// deployment keeps the storage its request named in (SaveToStorageID) and
// filled it with the default local storage, where every deployment's model
// lived before storages.
func migrateDeploymentWeightStorage(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&Deployment{}, "SaveToStorageID") {
		if err := db.Migrator().AddColumn(&Deployment{}, "SaveToStorageID"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasIndex(&Deployment{}, "SaveToStorageID") {
		if err := db.Migrator().CreateIndex(&Deployment{}, "SaveToStorageID"); err != nil {
			return err
		}
	}
	defaultID, err := migrationDefaultLocalStorageID(db)
	if err != nil || defaultID == 0 {
		return err
	}
	return db.Exec(`UPDATE deployments SET weight_storage_id = ? WHERE weight_storage_id IS NULL OR weight_storage_id = 0`, defaultID).Error
}

// dropModelsRootAndBuiltin is migration 125. A node's models directory is
// now the default local storage's override for that node (migration 122), and
// "built-in" no longer means anything.
func dropModelsRootAndBuiltin(db *gorm.DB) error {
	if err := dropColumns(db, "nodes", "models_root"); err != nil {
		return err
	}
	return dropColumns(db, "storages", "builtin")
}

// migrateStoragePathsExplicit is migration 126: storages stop deferring to
// the agent's directories. A local storage without a directory used each
// agent's models directory under its state directory, and an nfs_cache
// storage without a cache directory used <state>/cache/<name>. Every node
// keeps the directory it reported: it becomes that node's override, unless
// the node already has one. The storage then states the default directory,
// which only nodes that never reported (or join later) use.
func migrateStoragePathsExplicit(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Storage{}) {
		return nil
	}
	var rows []Storage
	if err := db.Where("(type = ? AND (path = '' OR path IS NULL)) OR (type = ? AND (cache_path = '' OR cache_path IS NULL))",
		proto.StorageLocal, proto.StorageNFSCache).Order("id").Find(&rows).Error; err != nil {
		return err
	}
	for _, st := range rows {
		cache := st.Type == proto.StorageNFSCache
		field := "path"
		if cache {
			field = "cache_path"
		}
		var states []StorageNodeState
		if err := db.Where("storage_id = ?", st.ID).Order("node_id").Find(&states).Error; err != nil {
			return err
		}
		for _, ns := range states {
			dir := strings.TrimSpace(ns.Path)
			if cache {
				dir = strings.TrimSpace(ns.CachePath)
			}
			if dir == "" || ns.NodeID == 0 {
				continue
			}
			var override StorageNode
			err := db.Where("storage_id = ? AND node_id = ?", st.ID, ns.NodeID).First(&override).Error
			switch {
			case err == nil:
				current := override.Path
				if cache {
					current = override.CachePath
				}
				if strings.TrimSpace(current) == "" {
					if err := db.Model(&StorageNode{}).Where("id = ?", override.ID).Update(field, dir).Error; err != nil {
						return err
					}
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				row := StorageNode{StorageID: st.ID, NodeID: ns.NodeID, Path: dir}
				if cache {
					row = StorageNode{StorageID: st.ID, NodeID: ns.NodeID, CachePath: dir}
				}
				// This migration predates override_only and must also run against
				// the historical table without that later model field.
				if err := db.Omit("OverrideOnly").Create(&row).Error; err != nil {
					return err
				}
			default:
				return err
			}
		}
		value := DefaultLocalStoragePath
		if cache {
			value = DefaultCachePath(st.Name)
		}
		if err := db.Model(&Storage{}).Where("id = ?", st.ID).Update(field, value).Error; err != nil {
			return err
		}
	}
	return nil
}
