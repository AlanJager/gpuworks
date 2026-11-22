package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// What the cluster upgrade observes: the phase, how far the database's
// schema has been followed, and which executable every member reported. The
// steps in cluster_upgrade.go decide from one such observation.

// ClusterUpgradeMember is one admitted member and the executable it reported.
type ClusterUpgradeMember struct {
	KeyFingerprint string
	Address        string
	// Reported is false for a member that has not started an executable
	// that reports; the rest is then empty. Markers and Last are the core
	// list's, Extensions counts the markers of its extension modules.
	Reported   bool
	Markers    int
	Last       string
	Extensions int
	// Fresh is true when the report was repeated within the last
	// clusterUpgradeReportFresh; a member repeats it while it waits.
	Fresh    bool
	position schemaPosition
}

// ClusterUpgradeStatus is one observation of the upgrade facts.
type ClusterUpgradeStatus struct {
	Phase MaintenancePhaseState
	// Applied counts the database's core markers and Known this
	// executable's; the Extensions pair counts the markers of the
	// executable's extension modules the same way.
	Applied           int
	Known             int
	AppliedExtensions int
	KnownExtensions   int
	// Base is the core marker count when the window began, zero outside one.
	Base    int
	Members []ClusterUpgradeMember
	// want is the position of the executable that made the observation,
	// applied the database's, and base the one recorded at begin.
	want, applied, base schemaPosition
}

// AppliedExtensionMarkers is how many markers the database holds in each
// extension namespace this executable knows; a namespace with none is absent.
// With Applied it says which schema the database has, so that a caller can
// tell whether a backup was taken at this schema.
func (s ClusterUpgradeStatus) AppliedExtensionMarkers() map[string]int {
	if len(s.applied.Sets) == 0 {
		return nil
	}
	markers := make(map[string]int, len(s.applied.Sets))
	for namespace, position := range s.applied.Sets {
		markers[namespace] = position.Markers
	}
	return markers
}

// Pending is how many migrations this executable would apply, core and
// extension modules together.
func (s ClusterUpgradeStatus) Pending() int {
	return s.Known - s.Applied + s.KnownExtensions - s.AppliedExtensions
}

// Ready reports that every admitted member last reported exactly this
// executable's migration lists, and that most of them repeated the report
// recently. A member that is down keeps its last report; one that came back
// on another executable has replaced it.
func (s ClusterUpgradeStatus) Ready() bool {
	if len(s.Members) == 0 || s.want.Markers == 0 {
		return false
	}
	fresh := 0
	for _, member := range s.Members {
		if !member.Reported || !member.position.equal(s.want) {
			return false
		}
		if member.Fresh {
			fresh++
		}
	}
	return fresh*2 > len(s.Members)
}

// pendingSchemaMigrations counts this executable's migrations the database
// has not applied. The applied markers must be a prefix of the executable's
// list by version and name; anything else is incompatible.

// ClusterUpgradeStatus reads the phase, the schema position and every
// member's report. It uses plain column lists, because the schema it reads
// may be older than this executable's entities.
func (s *Store) ClusterUpgradeStatus(ctx context.Context) (ClusterUpgradeStatus, error) {
	return s.clusterUpgrader().status(ctx)
}

func (u clusterUpgrader) status(ctx context.Context) (ClusterUpgradeStatus, error) {
	// Plain reads, not one transaction: on the replicated store a
	// transaction waits for the writer, which a running migration holds.
	// Each step acts through its own conditional write.
	return readClusterUpgradeStatus(ctx, u.s.db.WithContext(ctx), u.list, u.sets)
}

func readClusterUpgradeStatus(ctx context.Context, tx *gorm.DB, list []migration, sets []MigrationSet) (ClusterUpgradeStatus, error) {
	status := ClusterUpgradeStatus{Known: len(list), want: positionOf(list, sets)}
	pending, err := pendingMigrationsOf(tx, list)
	if err != nil {
		return status, err
	}
	status.Applied = status.Known - pending
	status.applied = positionOf(list[:status.Applied], nil)
	if status.applied.Sets, err = appliedNamespacePositions(tx, sets); err != nil {
		return status, err
	}
	status.KnownExtensions, status.AppliedExtensions = status.want.extensionMarkers(), status.applied.extensionMarkers()
	if status.Phase, err = (&Store{db: tx}).ReadMaintenancePhase(ctx); err != nil {
		return status, err
	}
	rows, err := tx.Raw(`SELECT key_fingerprint, address FROM cluster_admissions WHERE revoked_at IS NULL ORDER BY address`).Rows()
	if err != nil {
		return status, err
	}
	for rows.Next() {
		var member ClusterUpgradeMember
		if err := rows.Scan(&member.KeyFingerprint, &member.Address); err != nil {
			return status, errors.Join(err, rows.Close())
		}
		status.Members = append(status.Members, member)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return status, err
	}
	for i := range status.Members {
		key, err := clusterUpgradeMemberKey(status.Members[i].KeyFingerprint)
		if err != nil {
			return status, err
		}
		member := &status.Members[i]
		var age int64
		if member.Reported, err = readClusterUpgradeSetting(tx, key, &member.position, &age); err != nil {
			return status, err
		}
		member.Markers, member.Last, member.Extensions = member.position.Markers, member.position.Last, member.position.extensionMarkers()
		member.Fresh = member.Reported && age >= -int64(clusterUpgradeReportFresh/time.Second) && age <= int64(clusterUpgradeReportFresh/time.Second)
	}
	var age int64
	if _, err := readClusterUpgradeSetting(tx, clusterUpgradeBaseSetting, &status.base, &age); err != nil {
		return status, err
	}
	status.Base = status.base.Markers
	return status, nil
}

// readClusterUpgradeSetting reads one upgrade record and its age in seconds
// by the database's clock.
func readClusterUpgradeSetting(tx *gorm.DB, key string, into *schemaPosition, age *int64) (bool, error) {
	rows, err := tx.Raw(`SELECT value, CAST(strftime('%s', 'now') AS INTEGER) - CAST(strftime('%s', updated_at) AS INTEGER) FROM settings WHERE key = ?`, key).Rows()
	if err != nil {
		return false, err
	}
	if !rows.Next() {
		return false, errors.Join(rows.Err(), rows.Close())
	}
	var value string
	var seconds *int64
	if err := rows.Scan(&value, &seconds); err != nil {
		return false, errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(value), into); err != nil || into.Markers <= 0 || into.Last == "" {
		return false, fmt.Errorf("cluster upgrade record %s is invalid", key)
	}
	*age = int64(clusterUpgradeReportFresh/time.Second) + 1
	if seconds != nil {
		*age = *seconds
	}
	return true, nil
}

// appliedNamespacePositions reads how far the database has followed each of
// the executable's extension sets. A namespace's markers must be a prefix of
// its set, and the database must hold no namespace the executable lacks: an
// executable without a module cannot stand in for one that has it.
func appliedNamespacePositions(tx *gorm.DB, sets []MigrationSet) (map[string]schemaPosition, error) {
	if !tx.Migrator().HasTable(&NamespaceMigration{}) {
		return nil, nil
	}
	var rows []NamespaceMigration
	if err := tx.Select("namespace", "version", "name").Order("namespace, version").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: read extension migration markers: %w", ErrSchemaIncompatible, err)
	}
	recorded := map[string][]NamespaceMigration{}
	for _, row := range rows {
		recorded[row.Namespace] = append(recorded[row.Namespace], row)
	}
	var positions map[string]schemaPosition
	for _, set := range sets {
		applied := recorded[set.Namespace]
		delete(recorded, set.Namespace)
		if len(applied) > len(set.Migrations) {
			return nil, fmt.Errorf("%w: namespace %s has %d markers, this executable knows %d", ErrSchemaIncompatible, set.Namespace, len(applied), len(set.Migrations))
		}
		for i, marker := range applied {
			if set.Migrations[i].Version != marker.Version || set.Migrations[i].Name != marker.Name {
				return nil, fmt.Errorf("%w: namespace %s migration %d differs from this executable", ErrSchemaIncompatible, set.Namespace, marker.Version)
			}
		}
		if len(applied) > 0 {
			if positions == nil {
				positions = map[string]schemaPosition{}
			}
			positions[set.Namespace] = schemaPosition{Markers: len(applied), Last: applied[len(applied)-1].Name}
		}
	}
	for namespace := range recorded {
		return nil, fmt.Errorf("%w: the database has extension namespace %s, which this executable does not include", ErrSchemaIncompatible, namespace)
	}
	return positions, nil
}
