package store

import "gorm.io/gorm"

// createSlurmClusters adds the Slurm cluster entries and the GPU node lists
// their agents report (SlurmCluster, SlurmGPUNode) and the column that ties a
// node to its entry (Node.SlurmClusterID), with the partial unique index that
// lets enrollment find a returning node by (entry, Slurm node name). Existing
// nodes read as joined from no entry.
func createSlurmClusters(db *gorm.DB) error {
	if err := db.AutoMigrate(&SlurmCluster{}, &SlurmGPUNode{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&Node{}, "SlurmClusterID") {
		if err := db.Migrator().AddColumn(&Node{}, "SlurmClusterID"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasColumn(&Node{}, "SlurmNode") {
		if err := db.Migrator().AddColumn(&Node{}, "SlurmNode"); err != nil {
			return err
		}
	}
	return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_node_slurm ON nodes (slurm_cluster_id, slurm_node) WHERE slurm_cluster_id > 0").Error
}
