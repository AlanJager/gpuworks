package store

import (
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentHealthCheckMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO deployments (id, name) VALUES (1, 'old')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 138)

	db := open("deployment-health-check")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"HealthCheck", "HealthCheckThresholdSec"} {
		if !db.Migrator().HasColumn(&Deployment{}, column) {
			t.Fatalf("%s was not added", column)
		}
	}
	// An existing deployment is not checked until turned on, and then waits
	// the default.
	var row struct {
		HealthCheck             bool
		HealthCheckThresholdSec int
	}
	if err := db.Raw("SELECT health_check, health_check_threshold_sec FROM deployments WHERE id = 1").Scan(&row).Error; err != nil || row.HealthCheck || row.HealthCheckThresholdSec != 60 {
		t.Fatalf("existing row = %+v err=%v", row, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 138).Error; err != nil || marker.Name != "deployment_health_check" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 138").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("deployment-health-check-rollback")
	want := errors.New("injected health_check failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "HealthCheck") || rollback.Migrator().HasColumn(&Deployment{}, "HealthCheckThresholdSec") {
		t.Fatal("a column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 138).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Deployment{}, "HealthCheckThresholdSec") {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}

func TestHealthCheckedDeploymentsListsOnlyTheOnesTurnedOn(t *testing.T) {
	st := newTestStore(t)
	on, err := st.CreateDeployment(&Deployment{Name: "on", ServedName: "on", ModelID: 3, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(&Deployment{Name: "off", ServedName: "off", State: "active"}); err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(4, "deployment")
	defer sub.Close()
	if err := st.SetDeploymentHealthCheck(on.ID, true, 15); err != nil {
		t.Fatal(err)
	}
	got, err := st.HealthCheckedDeployments()
	if err != nil || len(got) != 1 || got[0].ID != on.ID || got[0].ModelID != 3 || got[0].ServedName != "on" || got[0].State != "active" || got[0].HealthCheckThresholdSec != 15 {
		t.Fatalf("checked = %+v err=%v", got, err)
	}
	select {
	case e := <-sub.C():
		if e.Key != uintKey(on.ID) || e.Reason != "health_check" {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Error("turning the check on published no change")
	}
	if err := st.SetDeploymentHealthCheck(on.ID, false, 15); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.HealthCheckedDeployments(); len(got) != 0 {
		t.Errorf("after turning off: %+v", got)
	}
}
