package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// joinedSchemaClass is how the replicated schema relates to this executable.
type joinedSchemaClass int

const (
	joinedSchemaCurrent joinedSchemaClass = iota
	// joinedSchemaLegacy is exactly the expanded runtime's v114 schema.
	joinedSchemaLegacy
	// joinedSchemaPending has applied a prefix of this executable's
	// migrations and waits for the cluster upgrade.
	joinedSchemaPending
)

// classifyJoinedSchema reads one replicated snapshot before a resumed member
// transfers ownership. Any valid phase classifies exact v115 as current;
// even serving is only an observation, not authority for a later write. A
// schema behind this executable is ErrSchemaUpgradePending: the member keeps
// serving the database and asks again.
func classifyJoinedSchema(ctx context.Context, tx *gorm.DB, expected ClusterIdentity) (legacy bool, err error) {
	class, err := classifyJoinedSchemaClass(ctx, tx, expected)
	if err != nil {
		return false, err
	}
	if class == joinedSchemaPending {
		return false, ErrSchemaUpgradePending
	}
	return class == joinedSchemaLegacy, nil
}

// classifyJoinedSchemaClass is classifyJoinedSchema for a reader that only
// observes the cluster, such as quorum facts, and so also answers while the
// schema is behind this executable.
func classifyJoinedSchemaClass(ctx context.Context, tx *gorm.DB, expected ClusterIdentity) (joinedSchemaClass, error) {
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return 0, ErrClusterIdentityConflict
	}
	class := joinedSchemaCurrent
	if err := checkLegacyExpandedSchema(tx); err == nil {
		class = joinedSchemaLegacy
	} else {
		if retryableClassificationRead(err) {
			return 0, err
		}
		if currentErr := checkCurrentSchema(tx); currentErr != nil {
			if retryableClassificationRead(currentErr) {
				return 0, currentErr
			}
			pending, err := pendingSchemaMigrations(tx)
			if err != nil || pending == 0 {
				return 0, currentErr
			}
			class = joinedSchemaPending
		}
		if _, err := (&Store{db: tx}).ReadMaintenancePhase(ctx); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrMaintenancePhaseUnavailable, err)
		}
	}
	actual, err := (&Store{db: tx}).ClusterIdentity(ctx)
	if err != nil {
		return 0, err
	}
	if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
		return 0, ErrClusterIdentityConflict
	}
	return class, nil
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
