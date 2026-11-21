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

// clusterUpgradeReportFresh is how recently a member must have repeated its
// report to count as present. A waiting member repeats it every second.
const clusterUpgradeReportFresh = 30 * time.Second

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

// schemaPosition is how far the migration lists have been followed: for the
// core list the number of markers and the name of the last one, and the same
// for every extension namespace that has a marker.
type schemaPosition struct {
	Markers int                       `json:"markers"`
	Last    string                    `json:"last"`
	Sets    map[string]schemaPosition `json:"sets,omitempty"`
}

func (p schemaPosition) equal(other schemaPosition) bool {
	if p.Markers != other.Markers || p.Last != other.Last || len(p.Sets) != len(other.Sets) {
		return false
	}
	for namespace, set := range p.Sets {
		if theirs, ok := other.Sets[namespace]; !ok || !set.equal(theirs) {
			return false
		}
	}
	return true
}

// extensionMarkers counts the markers of every namespace.
func (p schemaPosition) extensionMarkers() int {
	total := 0
	for _, set := range p.Sets {
		total += set.Markers
	}
	return total
}

func positionOf(list []migration, sets []MigrationSet) schemaPosition {
	position := schemaPosition{Markers: len(list)}
	if len(list) > 0 {
		position.Last = list[len(list)-1].Name
	}
	for _, set := range sets {
		if n := len(set.Migrations); n > 0 {
			if position.Sets == nil {
				position.Sets = map[string]schemaPosition{}
			}
			position.Sets[set.Namespace] = schemaPosition{Markers: n, Last: set.Migrations[n-1].Name}
		}
	}
	return position
}

// clusterUpgrader performs the upgrade steps for one executable's schema:
// the core migration list and the migration sets of its extension modules.
type clusterUpgrader struct {
	s    *Store
	list []migration
	sets []MigrationSet
}

func (s *Store) clusterUpgrader() clusterUpgrader {
	return clusterUpgrader{s: s, list: migrations, sets: s.extensions}
}

// ClusterUpgradeFor is the Store's upgrade steps for an executable that
// includes these extension migration sets. A Store bound before its
// extension schemas were adopted does not know them otherwise.
func (s *Store) ClusterUpgradeFor(sets []MigrationSet) ClusterUpgradeSteps {
	return ClusterUpgradeSteps{u: clusterUpgrader{s: s, list: migrations, sets: sets}}
}

// ClusterUpgradeSteps are the upgrade steps bound to one executable's schema.
type ClusterUpgradeSteps struct{ u clusterUpgrader }

func (c ClusterUpgradeSteps) Status(ctx context.Context) (ClusterUpgradeStatus, error) {
	return c.u.status(ctx)
}
func (c ClusterUpgradeSteps) Report(ctx context.Context, memberKey string) error {
	return c.u.report(ctx, memberKey)
}
func (c ClusterUpgradeSteps) Begin(ctx context.Context) (ClusterUpgradeStatus, error) {
	return c.u.begin(ctx)
}
func (c ClusterUpgradeSteps) Migrate(ctx context.Context) (ClusterUpgradeStatus, error) {
	return c.u.migrate(ctx)
}
func (c ClusterUpgradeSteps) Finish(ctx context.Context) (ClusterUpgradeStatus, error) {
	return c.u.finish(ctx)
}
func (c ClusterUpgradeSteps) Abort(ctx context.Context) (ClusterUpgradeStatus, error) {
	return c.u.abort(ctx)
}

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
	value, err := json.Marshal(positionOf(u.list, u.sets))
	if err != nil {
		return err
	}
	// The time is the database's, so every member's report is dated by one
	// clock. A repeated report is rewritten only once its date is half the
	// freshness bound old.
	return s.db.WithContext(ctx).Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
		WHERE settings.value != excluded.value OR COALESCE(strftime('%s', 'now') - strftime('%s', settings.updated_at), ?) >= ?`,
		key, string(value), int(clusterUpgradeReportFresh/time.Second), int(clusterUpgradeReportFresh/time.Second)/2).Error
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
		status, err = readClusterUpgradeStatus(ctx, tx, u.list, u.sets)
		return err
	})
	return status, err
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

// refuseNotReady says which members keep a step from running.
func refuseNotReady(status ClusterUpgradeStatus) error {
	var differ, silent []string
	for _, member := range status.Members {
		switch {
		case !member.Reported || !member.position.equal(status.want):
			differ = append(differ, member.Address)
		case !member.Fresh:
			silent = append(silent, member.Address)
		}
	}
	if len(differ) > 0 {
		return refuseClusterUpgrade("these members have not reported this executable's migrations (%d core, %d of extension modules): %s",
			status.Known, status.KnownExtensions, strings.Join(differ, ", "))
	}
	return refuseClusterUpgrade("fewer than half of the members repeated their report in the last %s; silent: %s", clusterUpgradeReportFresh, strings.Join(silent, ", "))
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
		return status, refuseNotReady(status)
	}
	if status.Phase.Phase == MaintenanceServing {
		value, err := json.Marshal(status.applied)
		if err != nil {
			return status, err
		}
		if err := s.schedulingWriter().WithContext(ctx).Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			clusterUpgradeBaseSetting, string(value), time.Now().UTC()).Error; err != nil {
			return status, err
		}
		status.Base, status.base = status.Applied, status.applied
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
		return status, refuseNotReady(status)
	}
	db := s.schedulingWriter().WithContext(ctx)
	if err := migrateDB(db, u.list); err != nil {
		return status, err
	}
	// The extension modules' own schemas follow the core's, as they do when
	// a single server starts.
	if err := validateMigrationSets(u.sets); err != nil {
		return status, err
	}
	if err := migrateNamespaces(db, u.sets); err != nil {
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
		return status, refuseNotReady(status)
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
	if status.Base == 0 || !status.applied.equal(status.base) {
		return status, refuseClusterUpgrade("the schema has moved in this window (core markers %d, now %d; extension markers %d, now %d); restore a backup to go back",
			status.base.Markers, status.Applied, status.base.extensionMarkers(), status.AppliedExtensions)
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
