package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func openBeforeUnitsAuto(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT, desired_state TEXT, nnodes INTEGER, placement TEXT)`,
		`INSERT INTO deployments (id, name, desired_state, nnodes, placement) VALUES (1, 'pd', 'active', 1, '{"pd":{"mode":"vllm-nixl"}}')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// An existing multi-node or PD deployment named its nodes itself and keeps
// that: the new column reads false.
func TestDeploymentUnitsAutoMigration(t *testing.T) {
	m := migrationByVersion(t, 178)
	db := openBeforeUnitsAuto(t, "units-auto")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "UnitsAuto") {
		t.Fatal("deployments.units_auto missing")
	}
	var dep struct{ UnitsAuto bool }
	if err := db.Raw(`SELECT units_auto FROM deployments WHERE id = 1`).Scan(&dep).Error; err != nil || dep.UnitsAuto {
		t.Fatalf("existing deployment = %+v err=%v, want manual", dep, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 178).Error; err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 178").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := openBeforeUnitsAuto(t, "units-auto-rollback")
	want := errors.New("injected failure")
	if err := applyMigration(rollback, migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "UnitsAuto") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 178").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
}

func TestSetDeploymentUnitsAuto(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", State: DeployActive, NNodes: 2, Placement: `{"tp":1,"pp":2}`})
	if err != nil {
		t.Fatal(err)
	}
	if dep.UnitsAuto {
		t.Fatal("a new deployment is automatic without saying so")
	}
	if err := st.SetDeploymentUnitsAuto(dep.ID, true, `{"tp":1,"pp":2,"units":[]}`); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(dep.ID)
	if !got.UnitsAuto || got.Placement != `{"tp":1,"pp":2,"units":[]}` {
		t.Fatalf("after switch = auto %v placement %s", got.UnitsAuto, got.Placement)
	}
	if err := st.SetDeploymentUnitsAuto(dep.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.UnitsAuto || got.Placement != `{"tp":1,"pp":2,"units":[]}` {
		t.Fatalf("switch back = auto %v placement %s, want the placement kept", got.UnitsAuto, got.Placement)
	}
	if err := st.SetDeploymentUnitsAuto(9999, true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment = %v", err)
	}
}
