package store

import "gorm.io/gorm"

// addNodeHostSystemUUID adds the firmware system UUID of each node's
// machine (Node.HostSystemUUID). Existing nodes start empty and fill in at
// their agent's next hello; an agent that cannot read it leaves it empty.
func addNodeHostSystemUUID(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "HostSystemUUID") {
		return nil
	}
	return db.Migrator().AddColumn(&Node{}, "HostSystemUUID")
}
