package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func openUpgradeCompatibilityDB(t *testing.T, statements ...string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "upgrade.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestOccupiedLegacyMigrationRepairsRequiredSchema(t *testing.T) {
	for _, version := range []int{27, 28, 40} {
		m := migrationByVersion(t, version)
		t.Run(m.Name, func(t *testing.T) {
			db := openUpgradeCompatibilityDB(t)
			if err := db.Create(&SchemaMigration{Version: version, Name: "historical_branch_migration"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			var model any
			switch version {
			case 27:
				model = &TerminalSession{}
			case 28:
				model = &Deployment{}
			case 40:
				model = &Instance{}
			}
			if !db.Migrator().HasTable(model) {
				t.Fatalf("occupied version %d left required table missing", version)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatalf("second start: %v", err)
			}
			var markers int64
			if err := db.Model(&SchemaMigration{}).Where("version = ?", version).Count(&markers).Error; err != nil || markers != 1 {
				t.Fatalf("marker count=%d err=%v", markers, err)
			}
		})
	}
}

func TestOccupiedLegacyMigrationRepairRollsBackAndRetries(t *testing.T) {
	db := openUpgradeCompatibilityDB(t)
	base := migrationByVersion(t, 40)
	if err := db.Create(&SchemaMigration{Version: base.Version, Name: "historical_branch_migration"}).Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected collision repair failure")
	failing := base
	failing.Apply = func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if db.Migrator().HasTable(&Instance{}) {
		t.Fatal("failed collision repair left schema behind")
	}
	var marker SchemaMigration
	if err := db.First(&marker, base.Version).Error; err != nil || marker.Name != "historical_branch_migration" {
		t.Fatalf("marker=%+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{base}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestInstanceRoleMigrationNormalizesPreviouslyAddedColumn(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, role TEXT)`,
		`INSERT INTO instances (id, role) VALUES (1, NULL), (2, ''), (3, 'prefill'), (4, 'decode')`,
	)
	m := migrationByVersion(t, 108)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var ordinary []int
	if err := db.Raw(`SELECT id FROM instances WHERE role = '' ORDER BY id`).Scan(&ordinary).Error; err != nil {
		t.Fatal(err)
	}
	if len(ordinary) != 2 || ordinary[0] != 1 || ordinary[1] != 2 {
		t.Fatalf("empty-role filter finds %v, want [1 2]", ordinary)
	}
	var roles []string
	if err := db.Raw(`SELECT role FROM instances WHERE id >= 3 ORDER BY id`).Scan(&roles).Error; err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 || roles[0] != "prefill" || roles[1] != "decode" {
		t.Fatalf("PD roles changed: %v", roles)
	}
}

func TestDeploymentDesiredStateMigrationBackfillsAlongsideLegacyState(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, state TEXT, desired_state TEXT)`,
		`INSERT INTO deployments (id, state, desired_state) VALUES (1, 'stopped', NULL), (2, 'active', ''), (3, 'stopped', 'deleting')`,
	)
	m := migrationByVersion(t, 74)
	for attempt := 0; attempt < 2; attempt++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
		var states []string
		if err := db.Raw(`SELECT desired_state FROM deployments ORDER BY id`).Scan(&states).Error; err != nil {
			t.Fatal(err)
		}
		if len(states) != 3 || states[0] != "stopped" || states[1] != "active" || states[2] != "deleting" {
			t.Fatalf("states=%v, want [stopped active deleting]", states)
		}
	}
}

func TestLegacyUpgradePreservesStoppedDeploymentAndOrdinaryInstance(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT)`,
		`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, uuid TEXT)`,
		`CREATE TABLE models (id INTEGER PRIMARY KEY, image_id INTEGER, source TEXT, source_id TEXT, revision TEXT, name TEXT)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, state TEXT, served_name TEXT)`,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT, deployment_id INTEGER, state TEXT)`,
		`INSERT INTO deployments (id, state, served_name) VALUES (1, 'stopped', 'legacy-stopped')`,
		`INSERT INTO instances (id, uuid, deployment_id, state) VALUES (1, 'legacy-ordinary', 1, 'stopped')`,
	)
	for _, m := range migrations {
		if m.Version > 26 {
			break
		}
		if err := db.Create(&SchemaMigration{Version: m.Version, Name: m.Name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: db}
	for attempt := 0; attempt < 2; attempt++ {
		if err := st.migrate(); err != nil {
			t.Fatalf("start %d: %v", attempt, err)
		}
		dep, err := st.GetDeployment(1)
		if err != nil || dep.State != DeployStopped {
			t.Fatalf("deployment=%+v err=%v", dep, err)
		}
		var ordinary int64
		if err := db.Model(&Instance{}).Where("uuid = ? AND role = ''", "legacy-ordinary").Count(&ordinary).Error; err != nil || ordinary != 1 {
			t.Fatalf("ordinary=%d err=%v", ordinary, err)
		}
	}
}

func TestLegacyValueRepairRecoversAlreadyRecordedMigrations(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, state TEXT, desired_state TEXT)`,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, role TEXT)`,
		`INSERT INTO deployments (id, state) VALUES (1, 'stopped')`,
		`INSERT INTO instances (id) VALUES (1)`,
	)
	for _, v := range []int{74, 108} {
		m := migrationByVersion(t, v)
		if err := db.Create(&SchemaMigration{Version: v, Name: m.Name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	repair := migrationByName(t, "legacy_instance_role_and_deployment_state")
	injected := errors.New("injected legacy backfill failure")
	failing := repair
	failing.Apply = func(tx *gorm.DB) error {
		if err := repair.Apply(tx); err != nil {
			return err
		}
		return injected
	}
	if err := applyMigration(db, failing); !errors.Is(err, injected) {
		t.Fatalf("error=%v", err)
	}
	var nulls int64
	if err := db.Raw("SELECT COUNT(*) FROM instances WHERE role IS NULL").Scan(&nulls).Error; err != nil || nulls != 1 {
		t.Fatalf("role backfill survived rollback: nulls=%d err=%v", nulls, err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", repair.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed marker=%d err=%v", markers, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := migrateDB(db, []migration{migrationByVersion(t, 74), migrationByVersion(t, 108), repair}); err != nil {
			t.Fatal(err)
		}
		var got struct {
			State string
			Role  string
		}
		if err := db.Raw(`SELECT d.desired_state AS state, i.role FROM deployments d, instances i`).Scan(&got).Error; err != nil || got.State != "stopped" || got.Role != "" {
			t.Fatalf("backfilled=%+v err=%v", got, err)
		}
	}
}

func TestOccupiedInstanceMigrationPreservesExistingRows(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT, state TEXT)`,
		`INSERT INTO instances (id, uuid, state) VALUES (7, 'legacy-instance', 'running')`,
	)
	m := migrationByVersion(t, 40)
	if err := db.Create(&SchemaMigration{Version: m.Version, Name: "historical_benchmark_migration"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Instance{}, "ImageDigestID") {
		t.Fatal("occupied migration left image_digest_id missing")
	}
	var inst Instance
	if err := db.First(&inst, 7).Error; err != nil || inst.UUID != "legacy-instance" || inst.State != "running" {
		t.Fatalf("instance=%+v err=%v", inst, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, m.Version).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker=%+v err=%v", marker, err)
	}
}
