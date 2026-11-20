package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

var ErrSchemaIncompatible = errors.New("database schema is not compatible with this executable")

// CheckCurrentSchema checks every migration marker without applying migrations,
// defaults, or process recovery. It is for offline operations and joining peers,
// not the normal upgrade path. Markers rely on atomic schema migrations; this
// does not diagnose arbitrary physical schema corruption.
func (s *Store) CheckCurrentSchema(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(checkCurrentSchema)
}

// CheckClusterCompatibility checks the authoritative database in one read
// snapshot. Ordinary connections may reach a remote leader: success does not
// prove local catch-up, quorum, admission, or permission to start business loops.
func (s *Store) CheckClusterCompatibility(ctx context.Context, expected ClusterIdentity) error {
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return ErrClusterIdentityConflict
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkCurrentSchema(tx); err != nil {
			return err
		}
		actual, err := (&Store{db: tx}).ClusterIdentity(ctx)
		if err != nil {
			return err
		}
		if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
			return ErrClusterIdentityConflict
		}
		return nil
	})
}

func checkCurrentSchema(db *gorm.DB) error {
	var applied []SchemaMigration
	// One extra row suffices to reject newer/unknown schemas without loading an
	// unbounded marker table. No AutoMigrate or write callback is used here.
	if err := db.Select("version", "name").Order("version").Limit(len(migrations) + 1).Find(&applied).Error; err != nil {
		return fmt.Errorf("%w: read migration markers: %w", ErrSchemaIncompatible, err)
	}
	if len(applied) != len(migrations) {
		return fmt.Errorf("%w: require %d migration markers, found %d", ErrSchemaIncompatible, len(migrations), len(applied))
	}
	expected := make(map[int]string, len(migrations))
	for _, m := range migrations {
		expected[m.Version] = m.Name
	}
	for _, m := range applied {
		name, known := expected[m.Version]
		if !known || m.Name != name {
			return fmt.Errorf("%w: migration %d differs from this executable", ErrSchemaIncompatible, m.Version)
		}
		delete(expected, m.Version)
	}
	if len(expected) != 0 {
		return ErrSchemaIncompatible
	}
	return nil
}
