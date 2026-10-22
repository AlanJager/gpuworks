package store

import (
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentTokenizeDisabledMigration(t *testing.T) {
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
	m := migrationByVersion(t, 165)

	db := open("deployment-tokenize")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "TokenizeDisabled") {
		t.Fatal("tokenize_disabled was not added")
	}
	// A deployment from before the switch keeps tokenize open.
	var row struct {
		TokenizeDisabled bool
	}
	if err := db.Raw("SELECT tokenize_disabled FROM deployments WHERE id = 1").Scan(&row).Error; err != nil || row.TokenizeDisabled {
		t.Fatalf("existing row = %+v err=%v", row, err)
	}
	// The column is NOT NULL with a false default: a row written without it
	// (an older binary's insert) is open too.
	if err := db.Exec("INSERT INTO deployments (id, name) VALUES (2, 'later')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT tokenize_disabled FROM deployments WHERE id = 2").Scan(&row).Error; err != nil || row.TokenizeDisabled {
		t.Fatalf("row without the column = %+v err=%v", row, err)
	}
	if err := db.Exec("INSERT INTO deployments (id, name, tokenize_disabled) VALUES (3, 'null', NULL)").Error; err == nil {
		t.Fatal("tokenize_disabled accepted NULL")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 165).Error; err != nil || marker.Name != "deployment_tokenize_disabled" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 165").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("deployment-tokenize-rollback")
	want := errors.New("injected tokenize_disabled failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "TokenizeDisabled") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 165).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Deployment{}, "TokenizeDisabled") {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	// A database without the deployments table yet has nothing to add to.
	empty, err := gorm.Open(sqlite.Open("file:deployment-tokenize-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&Deployment{}) {
		t.Fatalf("without deployments: err=%v", err)
	}
}

// SetDeploymentTokenize stores the switch inverted and publishes the change
// the gateway's route table rebuilds on; a new deployment is open.
func TestSetDeploymentTokenize(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeployment(&Deployment{Name: "tok", ServedName: "tok", State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := st.GetDeployment(dep.ID); row.TokenizeDisabled {
		t.Fatalf("a new deployment is closed: %+v", row)
	}
	sub := st.Bus.Subscribe(4, "deployment")
	defer sub.Close()
	if err := st.SetDeploymentTokenize(dep.ID, false); err != nil {
		t.Fatal(err)
	}
	if row, _ := st.GetDeployment(dep.ID); !row.TokenizeDisabled {
		t.Fatalf("closing did not store: %+v", row)
	}
	select {
	case e := <-sub.C():
		if e.Key != uintKey(dep.ID) || e.Reason != "tokenize" {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Error("closing tokenize published no change")
	}
	routed, err := st.RoutingDeployments()
	if err != nil || len(routed) != 1 || !routed[0].TokenizeDisabled {
		t.Fatalf("the route table's read does not carry the switch: %+v err=%v", routed, err)
	}
	if err := st.SetDeploymentTokenize(dep.ID, true); err != nil {
		t.Fatal(err)
	}
	if row, _ := st.GetDeployment(dep.ID); row.TokenizeDisabled {
		t.Fatalf("opening did not store: %+v", row)
	}
	// A deployment created closed stays closed: GORM writes a true bool.
	closed, err := st.CreateDeployment(&Deployment{Name: "closed", ServedName: "closed", State: DeployActive, TokenizeDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := st.GetDeployment(closed.ID); !row.TokenizeDisabled {
		t.Fatalf("created closed = %+v", row)
	}
}
