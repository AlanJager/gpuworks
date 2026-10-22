package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func timelineIndexMigration(t *testing.T) migration {
	t.Helper()
	for _, m := range migrations {
		if m.Name == "instance_event_timeline_index" {
			return m
		}
	}
	t.Fatal("instance_event_timeline_index migration is missing")
	return migration{}
}

func timelineIndexNames(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var names []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'instance_events'").Scan(&names).Error; err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	return found
}

func TestInstanceEventTimelineIndexMigrationCreatesBothIndexesIdempotently(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:timeline-index-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &InstanceEvent{}); err != nil {
		t.Fatal(err)
	}
	m := timelineIndexMigration(t)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	found := timelineIndexNames(t, db)
	for _, want := range []string{"idx_instance_events_uuid_id", "idx_instance_events_deployment_id_id"} {
		if !found[want] {
			t.Fatalf("index %s was not created; have %v", want, found)
		}
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", m.Version).First(&marker).Error; err != nil {
		t.Fatalf("migration marker missing: %v", err)
	}
	if marker.Name != m.Name {
		t.Fatalf("marker name = %q, want %q", marker.Name, m.Name)
	}
	// Re-running must neither fail nor duplicate the marker.
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count = %d err = %v", markers, err)
	}
}

// A database whose instance_events table has not been created yet (a fixture
// carrying only the tables it exercises) must be left alone rather than fail.
func TestInstanceEventTimelineIndexMigrationSkipsAMissingTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:timeline-index-no-table?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{timelineIndexMigration(t)}); err != nil {
		t.Fatalf("migration on a database without instance_events: %v", err)
	}
	if db.Migrator().HasTable(&InstanceEvent{}) {
		t.Fatal("the migration created the table it was supposed to skip")
	}
}

// The second index failing must leave neither the first index nor the marker
// behind, so a corrected retry starts from the same place.
func TestInstanceEventTimelineIndexMigrationRollsBackBothOnFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:timeline-index-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &InstanceEvent{}); err != nil {
		t.Fatal(err)
	}
	real := timelineIndexMigration(t)
	injected := errors.New("injected index failure")
	err = applyMigration(db, migration{Version: real.Version, Name: real.Name, Apply: func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_instance_events_uuid_id ON instance_events (instance_uuid, id)").Error; err != nil {
			return err
		}
		return injected
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("applyMigration error = %v, want the injected failure", err)
	}
	if found := timelineIndexNames(t, db); found["idx_instance_events_uuid_id"] {
		t.Fatalf("the first index survived a failed migration: %v", found)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", real.Version).First(&marker).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("marker after rollback = %+v err = %v", marker, err)
	}
	if err := migrateDB(db, []migration{real}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	found := timelineIndexNames(t, db)
	if !found["idx_instance_events_uuid_id"] || !found["idx_instance_events_deployment_id_id"] {
		t.Fatalf("retry did not create both indexes: %v", found)
	}
}
