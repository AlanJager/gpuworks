package store

import "gorm.io/gorm"

func migrateClusterAuthorizations(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE cluster_authorizations (
 id INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
 revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision >= 0)
 )`).Error; err != nil {
		return err
	}
	return db.Exec(`INSERT INTO cluster_authorizations(id, revision)
 VALUES (1, CASE WHEN EXISTS (SELECT 1 FROM cluster_admissions) THEN 1 ELSE 0 END)`).Error
}
