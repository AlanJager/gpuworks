package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// classifyJoinedSchema reads one replicated snapshot before a resumed member
// transfers ownership. Any valid phase classifies exact v115 as current;
// even serving is only an observation, not authority for a later write.
func classifyJoinedSchema(ctx context.Context, tx *gorm.DB, expected ClusterIdentity) (legacy bool, err error) {
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return false, ErrClusterIdentityConflict
	}
	if err := checkLegacyExpandedSchema(tx); err == nil {
		legacy = true
	} else {
		if retryableClassificationRead(err) {
			return false, err
		}
		if err := checkCurrentSchema(tx); err != nil {
			return false, err
		}
		if _, err := (&Store{db: tx}).ReadMaintenancePhase(ctx); err != nil {
			return false, fmt.Errorf("%w: %w", ErrMaintenancePhaseUnavailable, err)
		}
	}
	actual, err := (&Store{db: tx}).ClusterIdentity(ctx)
	if err != nil {
		return false, err
	}
	if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
		return false, ErrClusterIdentityConflict
	}
	return legacy, nil
}

// These errors mean the snapshot was not observed, rather than that its
// migration markers or current admission were incompatible.
func retryableClassificationRead(err error) bool {
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone)
}

func checkLegacyExpandedSchema(db *gorm.DB) error {
	const legacyVersion = 114
	var applied []SchemaMigration
	if err := db.Select("version", "name").Order("version").Limit(len(migrations) + 1).Find(&applied).Error; err != nil {
		return fmt.Errorf("%w: read migration markers: %w", ErrSchemaIncompatible, err)
	}
	legacyCount := 0
	for _, migration := range migrations {
		if migration.Version > legacyVersion {
			break
		}
		legacyCount++
	}
	if legacyCount == 0 || migrations[legacyCount-1].Version != legacyVersion || len(applied) != legacyCount {
		return fmt.Errorf("%w: legacy expanded runtime requires exactly v114 markers", ErrSchemaIncompatible)
	}
	for i, migration := range migrations[:legacyCount] {
		if applied[i].Version != migration.Version || applied[i].Name != migration.Name {
			return fmt.Errorf("%w: migration %d differs from v114", ErrSchemaIncompatible, migration.Version)
		}
	}
	var phaseObjects int
	if err := db.Raw(`SELECT count(*) FROM sqlite_schema WHERE name = 'maintenance_phase' OR name LIKE 'maintenance_phase_%'`).Row().Scan(&phaseObjects); err != nil {
		return fmt.Errorf("%w: inspect phase schema: %w", ErrSchemaIncompatible, err)
	}
	if phaseObjects != 0 {
		return fmt.Errorf("%w: maintenance phase schema exists without v115 marker", ErrSchemaIncompatible)
	}
	return nil
}
