package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// openBeforeResidueAndReplacement is a database from before per-instance
// sleep residues and node replacement: deployments and instances without
// their columns, one row each.
func openBeforeResidueAndReplacement(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT, desired_state TEXT, sleepable NUMERIC, replicas INTEGER)`,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT, deployment_id INTEGER, state TEXT, sleep_level INTEGER)`,
		`INSERT INTO deployments (id, name, desired_state, sleepable, replicas) VALUES (1, 'old', 'active', 1, 2)`,
		`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level) VALUES (1, 'old-1', 1, 'running', 2)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestSleepResidueAndNodeReplaceMigrations(t *testing.T) {
	ms := []migration{migrationByVersion(t, 176), migrationByVersion(t, 177)}
	db := openBeforeResidueAndReplacement(t, "residue-node-replace")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Instance{}, "SleepResidueBytes") {
		t.Fatal("instances.sleep_residue_bytes missing")
	}
	if !db.Migrator().HasColumn(&Deployment{}, "NodeReplaceAfterSec") {
		t.Fatal("deployments.node_replace_after_sec missing")
	}
	// Existing rows: nothing measured, and replicas move off a node offline
	// for ten minutes.
	var inst struct{ SleepResidueBytes int64 }
	if err := db.Raw(`SELECT sleep_residue_bytes FROM instances WHERE id = 1`).Scan(&inst).Error; err != nil || inst.SleepResidueBytes != 0 {
		t.Fatalf("existing instance = %+v err=%v", inst, err)
	}
	var dep struct{ NodeReplaceAfterSec int }
	if err := db.Raw(`SELECT node_replace_after_sec FROM deployments WHERE id = 1`).Scan(&dep).Error; err != nil || dep.NodeReplaceAfterSec != 600 {
		t.Fatalf("existing deployment = %+v err=%v", dep, err)
	}
	for _, version := range []int{176, 177} {
		var marker SchemaMigration
		if err := db.First(&marker, version).Error; err != nil {
			t.Fatalf("marker %d: %v", version, err)
		}
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version IN (176, 177)").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without markers: %v", err)
	}

	// A failure inside each migration leaves neither its schema nor its marker.
	for _, m := range ms {
		rollback := openBeforeResidueAndReplacement(t, "residue-node-replace-rollback-"+m.Name)
		want := errors.New("injected " + m.Name + " failure")
		failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
			if err := m.Apply(tx); err != nil {
				return err
			}
			return want
		}}
		if err := applyMigration(rollback, failing); !errors.Is(err, want) {
			t.Fatalf("%s error = %v", m.Name, err)
		}
		switch m.Version {
		case 176:
			if rollback.Migrator().HasColumn(&Instance{}, "SleepResidueBytes") {
				t.Fatal("the instance column survived the failed migration")
			}
		case 177:
			if rollback.Migrator().HasColumn(&Deployment{}, "NodeReplaceAfterSec") {
				t.Fatal("the deployment column survived the failed migration")
			}
		}
		var count int64
		if err := rollback.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s marker count = %d err=%v", m.Name, count, err)
		}
		if err := migrateDB(rollback, ms); err != nil {
			t.Fatalf("restart after %s rollback: %v", m.Name, err)
		}
	}
}
