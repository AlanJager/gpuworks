package store

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// pullRulesBeforeControlPlane is pull_source_rules as migration 152 created it.
func pullRulesBeforeControlPlane(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE pull_source_rules (id INTEGER PRIMARY KEY, position INTEGER, match TEXT, allow_upstream NUMERIC, note TEXT, created_at DATETIME, updated_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO pull_source_rules (id, position, match, allow_upstream, note) VALUES (1, 0, 'docker.io', 1, 'hub proxy')`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// Every existing rule is left out of the control plane's reads; applying the
// change again changes nothing.
func TestControlPlaneMigrationAddsTheChoiceOff(t *testing.T) {
	db := pullRulesBeforeControlPlane(t)
	m := migrationByName(t, "pull_source_rules_control_plane")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("pull_source_rules", "control_plane") {
		t.Fatal("control_plane is missing after the migration")
	}
	var row struct {
		Match        string
		ControlPlane bool
	}
	if err := db.Raw(`SELECT match, COALESCE(control_plane, 0) AS control_plane FROM pull_source_rules WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Match != "docker.io" || row.ControlPlane {
		t.Fatalf("existing rule after the migration: %+v", row)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// Schema and marker are one transaction: a failure leaves neither, and the
// retry completes.
func TestControlPlaneMigrationRollsBackAndCanRetry(t *testing.T) {
	db := pullRulesBeforeControlPlane(t)
	m := migrationByName(t, "pull_source_rules_control_plane")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_control_plane_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("pull_source_rules", "control_plane") {
		t.Fatal("the column survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_control_plane_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("pull_source_rules", "control_plane") {
		t.Fatal("the retry did not add the column")
	}
}
