package store

import "gorm.io/gorm"

func migrateClusterInvitationOrigin(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ClusterInvitation{}) {
		return ErrClusterNotInitialized
	}
	if db.Migrator().HasColumn(&ClusterInvitation{}, "AuthorizedEnrollmentOrigin") {
		return nil
	}
	return db.Exec("ALTER TABLE cluster_invitations ADD COLUMN authorized_enrollment_origin TEXT NOT NULL DEFAULT ''").Error
}
