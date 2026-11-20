package store

import (
	"time"

	"gorm.io/gorm"
)

// Keep the original admission schema fixed as the current entity evolves.
type initialClusterInvitation struct {
	TokenHash  string `gorm:"primaryKey;size:64"`
	ClusterID  string `gorm:"size:32;not null;index"`
	IssuedBy   uint   `gorm:"not null"`
	CreatedAt  time.Time
	ExpiresAt  time.Time `gorm:"not null"`
	ConsumedAt *time.Time
}

func (initialClusterInvitation) TableName() string { return "cluster_invitations" }

type initialClusterAdmission struct {
	KeyFingerprint string `gorm:"primaryKey;size:64"`
	InvitationHash string `gorm:"size:64;not null;uniqueIndex"`
	Address        string `gorm:"size:255;not null;uniqueIndex:idx_cluster_admission_address,where:revoked_at IS NULL"`
	AdmittedAt     time.Time
	RevokedAt      *time.Time
}

func (initialClusterAdmission) TableName() string { return "cluster_admissions" }

func migrateClusterAdmission(db *gorm.DB) error {
	return db.AutoMigrate(&ClusterIdentity{}, &initialClusterInvitation{}, &initialClusterAdmission{})
}

func migrateClusterMaterialGrants(db *gorm.DB) error {
	if db.Migrator().HasColumn(&ClusterAdmission{}, "MaterialGrantSequence") {
		return nil
	}
	return db.Exec(`ALTER TABLE cluster_admissions ADD COLUMN material_grant_sequence integer NOT NULL DEFAULT 0 CONSTRAINT cluster_material_grant_sequence CHECK (material_grant_sequence >= 0)`).Error
}
