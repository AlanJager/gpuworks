package store

import "gorm.io/gorm"

// addNodeSlurmState adds what a Slurm node's agent reports about Slurm's
// willingness to run jobs on it (Node.SlurmNode, SlurmUnavailable,
// SlurmReason). Existing nodes read as not on Slurm until their agent
// reports again.
func addNodeSlurmState(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, col := range []string{"SlurmNode", "SlurmUnavailable", "SlurmReason"} {
		if !db.Migrator().HasColumn(&Node{}, col) {
			if err := db.Migrator().AddColumn(&Node{}, col); err != nil {
				return err
			}
		}
	}
	return nil
}
