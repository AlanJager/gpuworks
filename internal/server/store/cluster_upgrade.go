package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// A cluster changes its schema in one coordinated step. Members are first
// replaced by an executable that knows more migrations than the database has
// applied; such a member serves the database and keeps its business closed.
// When every member has reported that executable, the leader takes the
// maintenance phase to upgrading, applies the migrations once, and returns the
// phase to serving. The phase closes business writes on every member for the
// whole step, including a member that still runs the earlier executable.
//
// Nothing here stores a target: the target is the migration list of the
// executable that performs a step, and every step requires every member to
// have reported that same list. A step interrupted by a leadership change is
// repeated on the new leader; applied migrations are skipped by their markers.

var (
	// ErrSchemaUpgradePending means the database has applied a prefix of this
	// executable's migrations. The member waits for the cluster upgrade.
	ErrSchemaUpgradePending = errors.New("database schema is older than this executable and awaits the cluster upgrade")
	// ErrClusterUpgradeRefused means a step's precondition does not hold.
	// Nothing was changed.
	ErrClusterUpgradeRefused = errors.New("cluster upgrade step refused")
)

const (
	// One row per member, keyed by a prefix of its key fingerprint that keeps
	// the key within the settings key length.
	clusterUpgradeMemberSetting = "cluster.upgrade.member:"
	// The schema position at which the window began; abort returns only there.
	clusterUpgradeBaseSetting = "cluster.upgrade.base"
)

// isClusterUpgradeSetting identifies the rows a cluster upgrade keeps in the
// settings table. They are not platform settings and are removed at its end.
func isClusterUpgradeSetting(key SettingKey) bool {
	return strings.HasPrefix(string(key), "cluster.upgrade.")
}

// schemaPosition is how far a migration list has been followed: the number of
// markers and the name of the last one.
type schemaPosition struct {
	Markers int    `json:"markers"`
	Last    string `json:"last"`
}

func positionOf(list []migration) schemaPosition {
	return schemaPosition{Markers: len(list), Last: list[len(list)-1].Name}
}

// clusterUpgrader performs the upgrade steps for one migration list: the
// executable's, or a test's.
type clusterUpgrader struct {
	s    *Store
	list []migration
}

func (s *Store) clusterUpgrader() clusterUpgrader { return clusterUpgrader{s: s, list: migrations} }

// ClusterUpgradeMember is one admitted member and the executable it reported.
type ClusterUpgradeMember struct {
	KeyFingerprint string
	Address        string
	// Reported is false for a member that has not started an executable
	// that reports; Markers and Last are then empty.
	Reported bool
	Markers  int
	Last     string
}

// ClusterUpgradeStatus is one observation of the upgrade facts.
type ClusterUpgradeStatus struct {
	Phase MaintenancePhaseState
	// Applied counts the database's markers and Known this executable's.
	Applied int
	Known   int
	// Base is the marker count when the window began, zero outside one.
	Base    int
	Members []ClusterUpgradeMember
	// want is the position of the executable that made the observation.
	want schemaPosition
}

// Pending is how many migrations this executable would apply.
func (s ClusterUpgradeStatus) Pending() int { return s.Known - s.Applied }

// Ready reports that every admitted member runs an executable with exactly
// this executable's migration list.
func (s ClusterUpgradeStatus) Ready() bool {
	want := s.want
	if len(s.Members) == 0 || want.Markers == 0 {
		return false
	}
	for _, member := range s.Members {
		if !member.Reported || member.Markers != want.Markers || member.Last != want.Last {
			return false
		}
	}
	return true
}

// pendingSchemaMigrations counts this executable's migrations the database
// has not applied. The applied markers must be a prefix of the executable's
// list by version and name; anything else is incompatible.
func pendingSchemaMigrations(db *gorm.DB) (int, error) {
	return pendingMigrationsOf(db, migrations)
}

func pendingMigrationsOf(db *gorm.DB, migrations []migration) (int, error) {
	var applied []SchemaMigration
	if err := db.Select("version", "name").Order("version").Limit(len(migrations) + 1).Find(&applied).Error; err != nil {
		return 0, fmt.Errorf("%w: read migration markers: %w", ErrSchemaIncompatible, err)
	}
	if len(applied) == 0 || len(applied) > len(migrations) {
		return 0, fmt.Errorf("%w: this executable knows %d migration markers, the database has %d", ErrSchemaIncompatible, len(migrations), len(applied))
	}
	for i, marker := range applied {
		if migrations[i].Version != marker.Version || migrations[i].Name != marker.Name {
			return 0, fmt.Errorf("%w: migration %d differs from this executable", ErrSchemaIncompatible, marker.Version)
		}
	}
	return len(migrations) - len(applied), nil
}

func clusterUpgradeMemberKey(fingerprint string) (string, error) {
	if !validClusterDigest(fingerprint, 32) {
		return "", ErrClusterAdmissionRejected
	}
	return clusterUpgradeMemberSetting + fingerprint[:40], nil
}

// ReportClusterUpgradeCapability records which migration list the member with
// this key runs. A member reports each time it finds the schema behind its
// executable, so a report always comes from an executable that waits.
func (s *Store) ReportClusterUpgradeCapability(ctx context.Context, memberKey string) error {
	return s.clusterUpgrader().report(ctx, memberKey)
}

func (u clusterUpgrader) report(ctx context.Context, memberKey string) error {
	s := u.s
	key, err := clusterUpgradeMemberKey(memberKey)
	if err != nil {
		return err
	}
	value, err := json.Marshal(positionOf(u.list))
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		WHERE settings.value != excluded.value`, key, string(value), time.Now().UTC()).Error
}

// ClusterUpgradeStatus reads the phase, the schema position and every
// member's report. It uses plain column lists, because the schema it reads
// may be older than this executable's entities.
func (s *Store) ClusterUpgradeStatus(ctx context.Context) (ClusterUpgradeStatus, error) {
	return s.clusterUpgrader().status(ctx)
}

func (u clusterUpgrader) status(ctx context.Context) (ClusterUpgradeStatus, error) {
	var status ClusterUpgradeStatus
	err := u.s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		status, err = readClusterUpgradeStatus(ctx, tx, u.list)
		return err
	})
	return status, err
}

func readClusterUpgradeStatus(ctx context.Context, tx *gorm.DB, list []migration) (ClusterUpgradeStatus, error) {
	status := ClusterUpgradeStatus{Known: len(list), want: positionOf(list)}
	pending, err := pendingMigrationsOf(tx, list)
	if err != nil {
		return status, err
	}
	status.Applied = status.Known - pending
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
		var reported schemaPosition
		found, err := readClusterUpgradeSetting(tx, key, &reported)
		if err != nil {
			return status, err
		}
		status.Members[i].Reported = found
		status.Members[i].Markers, status.Members[i].Last = reported.Markers, reported.Last
	}
	var base schemaPosition
	if _, err := readClusterUpgradeSetting(tx, clusterUpgradeBaseSetting, &base); err != nil {
		return status, err
	}
	status.Base = base.Markers
	return status, nil
}

func readClusterUpgradeSetting(tx *gorm.DB, key string, into *schemaPosition) (bool, error) {
	var values []string
	if err := tx.Raw(`SELECT value FROM settings WHERE key = ?`, key).Scan(&values).Error; err != nil {
		return false, err
	}
	if len(values) != 1 {
		return false, nil
	}
	if err := json.Unmarshal([]byte(values[0]), into); err != nil || into.Markers <= 0 || into.Last == "" {
		return false, fmt.Errorf("cluster upgrade record %s is invalid", key)
	}
	return true, nil
}

func refuseClusterUpgrade(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrClusterUpgradeRefused, fmt.Sprintf(format, args...))
}

// BeginClusterUpgrade closes business on every member by taking the phase
// from serving to upgrading. It requires migrations to apply and every member
// reporting this executable. It continues a begin that stopped part way, and
// is confined to the database leader.
func (s *Store) BeginClusterUpgrade(ctx context.Context) (ClusterUpgradeStatus, error) {
	return s.clusterUpgrader().begin(ctx)
}

func (u clusterUpgrader) begin(ctx context.Context) (ClusterUpgradeStatus, error) {
	s := u.s
	status, err := u.status(ctx)
	if err != nil {
		return status, err
	}
	if status.Phase.Phase == MaintenanceUpgrading {
		return status, nil
	}
	if status.Phase.Phase == MaintenanceRecovering {
		return status, refuseClusterUpgrade("an earlier upgrade is finishing (phase recovering); finish it first")
	}
	if status.Pending() == 0 {
		return status, refuseClusterUpgrade("the database already has every migration of this executable; no window is needed")
	}
	if !status.Ready() {
		return status, refuseClusterUpgrade("not every member has reported this executable's %d migrations", status.Known)
	}
	if status.Phase.Phase == MaintenanceServing {
		value, err := json.Marshal(positionOf(u.list[:status.Applied]))
		if err != nil {
			return status, err
		}
		if err := s.schedulingWriter().WithContext(ctx).Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			clusterUpgradeBaseSetting, string(value), time.Now().UTC()).Error; err != nil {
			return status, err
		}
		status.Base = status.Applied
	}
	isolated := make([]string, 0, len(status.Members))
	for _, member := range status.Members {
		isolated = append(isolated, member.KeyFingerprint)
	}
	for status.Phase.Phase != MaintenanceUpgrading {
		switch status.Phase.Phase {
		case MaintenanceServing:
			status.Phase, err = s.TransitionMaintenancePhase(ctx, status.Phase, MaintenanceFreezing)
		case MaintenanceFreezing:
			status.Phase, err = s.TransitionMaintenancePhase(ctx, status.Phase, MaintenanceFrozen)
		case MaintenanceFrozen:
			// Every member reported an executable that waits with its
			// business closed, which is what isolates it.
			status.Phase, err = s.EstablishSafeMaintenancePoint(ctx, status.Phase, isolated)
		case MaintenanceDrained:
			status.Phase, err = s.TransitionMaintenancePhase(ctx, status.Phase, MaintenanceUpgrading)
		default:
			err = ErrMaintenanceTransitionInvalid
		}
		if err != nil {
			return status, err
		}
	}
	return status, nil
}

// MigrateClusterSchema applies this executable's pending migrations once, on
// the database leader, while the phase is upgrading. Each migration commits
// with its marker, so a repeated call after an interruption applies only what
// is left.
func (s *Store) MigrateClusterSchema(ctx context.Context) (ClusterUpgradeStatus, error) {
	return s.clusterUpgrader().migrate(ctx)
}

func (u clusterUpgrader) migrate(ctx context.Context) (ClusterUpgradeStatus, error) {
	s := u.s
	status, err := u.status(ctx)
	if err != nil {
		return status, err
	}
	if status.Phase.Phase != MaintenanceUpgrading {
		return status, refuseClusterUpgrade("migrations are applied only in phase upgrading, the phase is %s", status.Phase.Phase)
	}
	if !status.Ready() {
		return status, refuseClusterUpgrade("not every member has reported this executable's %d migrations", status.Known)
	}
	if err := migrateDB(s.schedulingWriter().WithContext(ctx), u.list); err != nil {
		return status, err
	}
	return u.status(ctx)
}

// FinishClusterUpgrade returns the phase to serving once the database has
// every migration of this executable and every member reports it, and removes
// the upgrade's records. Members open business again on their own.
func (s *Store) FinishClusterUpgrade(ctx context.Context) (ClusterUpgradeStatus, error) {
	return s.clusterUpgrader().finish(ctx)
}

func (u clusterUpgrader) finish(ctx context.Context) (ClusterUpgradeStatus, error) {
	s := u.s
	status, err := u.status(ctx)
	if err != nil {
		return status, err
	}
	if status.Phase.Phase == MaintenanceServing {
		return status, nil
	}
	if status.Phase.Phase != MaintenanceUpgrading && status.Phase.Phase != MaintenanceRecovering {
		return status, refuseClusterUpgrade("the phase is %s; only an upgrade that reached upgrading is finished", status.Phase.Phase)
	}
	if status.Pending() != 0 {
		return status, refuseClusterUpgrade("%d migrations are not applied", status.Pending())
	}
	if !status.Ready() {
		return status, refuseClusterUpgrade("not every member has reported this executable's %d migrations", status.Known)
	}
	return s.reopenAfterClusterUpgrade(ctx, status)
}

// AbortClusterUpgrade returns the phase to serving without a schema change.
// It is refused once a migration has been applied: the earlier executables
// could not serve that schema, so the way back is then a restored backup.
func (s *Store) AbortClusterUpgrade(ctx context.Context) (ClusterUpgradeStatus, error) {
	return s.clusterUpgrader().abort(ctx)
}

func (u clusterUpgrader) abort(ctx context.Context) (ClusterUpgradeStatus, error) {
	s := u.s
	status, err := u.status(ctx)
	if err != nil {
		return status, err
	}
	switch status.Phase.Phase {
	case MaintenanceServing:
		return status, nil
	case MaintenanceFreezing, MaintenanceFrozen, MaintenanceDrained:
		if status.Phase, err = s.TransitionMaintenancePhase(ctx, status.Phase, MaintenanceServing); err != nil {
			return status, err
		}
		return status, s.removeClusterUpgradeRecords(ctx)
	}
	if status.Base == 0 || status.Applied != status.Base {
		return status, refuseClusterUpgrade("the schema moved from %d to %d markers in this window; restore a backup to go back", status.Base, status.Applied)
	}
	return s.reopenAfterClusterUpgrade(ctx, status)
}

func (s *Store) reopenAfterClusterUpgrade(ctx context.Context, status ClusterUpgradeStatus) (ClusterUpgradeStatus, error) {
	var err error
	if status.Phase.Phase == MaintenanceUpgrading {
		if status.Phase, err = s.TransitionMaintenancePhase(ctx, status.Phase, MaintenanceRecovering); err != nil {
			return status, err
		}
	}
	if status.Phase, err = s.TransitionMaintenancePhase(ctx, status.Phase, MaintenanceServing); err != nil {
		return status, err
	}
	status.Base = 0
	return status, s.removeClusterUpgradeRecords(ctx)
}

func (s *Store) removeClusterUpgradeRecords(ctx context.Context) error {
	return s.schedulingWriter().WithContext(ctx).Exec(`DELETE FROM settings WHERE key = ? OR key LIKE ?`,
		clusterUpgradeBaseSetting, clusterUpgradeMemberSetting+"%").Error
}
