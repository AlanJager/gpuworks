package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func legacySyncGroups(t *testing.T) (*gorm.DB, []SyncGroup) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&SchemaMigration{}, &SyncGroup{}, &SyncGroupRequirement{}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("组", 21) + "a"
	groups := []SyncGroup{
		{ID: 1, Name: " same ", RateLimitKiB: 10}, {ID: 2, Name: "same", RateLimitKiB: 20},
		{ID: 3, Name: long, RateLimitKiB: 30}, {ID: 4, Name: long, RateLimitKiB: 40},
		{ID: 5, Name: "same (#2)", RateLimitKiB: 50},
		{ID: 6, Name: DefaultSyncGroupName}, {ID: 7, Name: DefaultSyncGroupName, RateLimitKiB: 70},
	}
	for i := range groups {
		if err := db.Create(&groups[i]).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&SyncGroupRequirement{GroupID: groups[i].ID, Key: "site", Value: fmt.Sprint(groups[i].ID)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Compare persisted timestamps rather than Go's in-memory Location
	// pointers (SQLite keeps the offset but not its original location name).
	if err := db.Order("id").Find(&groups).Error; err != nil {
		t.Fatal(err)
	}
	return db, groups
}

func TestSyncGroupNameMigrationPreservesGroupsAndIsIdempotent(t *testing.T) {
	db, original := legacySyncGroups(t)
	m := migrationByVersion(t, 198)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var got []SyncGroup
	if err := db.Order("id").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) {
		t.Fatalf("groups lost: %v", got)
	}
	names := map[string]bool{}
	for i, row := range got {
		if names[row.Name] || len(row.Name) > syncGroupNameMaxBytes || !utf8.ValidString(row.Name) {
			t.Fatalf("invalid unique name: %q", row.Name)
		}
		names[row.Name] = true
		before := original[i]
		before.Name = row.Name
		if !reflect.DeepEqual(row, before) {
			t.Fatalf("group data changed: got=%+v want=%+v", row, before)
		}
		var requirement SyncGroupRequirement
		if err := db.First(&requirement, "group_id = ?", row.ID).Error; err != nil || requirement.Value != fmt.Sprint(row.ID) {
			t.Fatalf("selector changed: %+v %v", requirement, err)
		}
	}
	if got[0].Name != "same" || got[1].Name != "same (#2-1)" || got[4].Name != "same (#2)" || got[5].Name != DefaultSyncGroupName {
		t.Fatalf("original names not protected: %+v", got)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 198).Error; err != nil || marker.Name != "sync_group_unique_names" {
		t.Fatalf("marker: %+v %v", marker, err)
	}
	if !db.Migrator().HasIndex(&SyncGroup{}, "idx_sync_groups_name") {
		t.Fatal("unique index missing")
	}
	if err := db.Create(&SyncGroup{Name: "same"}).Error; err == nil {
		t.Fatal("index accepted duplicate")
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var again []SyncGroup
	if err := db.Order("id").Find(&again).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("migration changed groups on rerun: %v", again)
	}
}

func TestSyncGroupNameMigrationRollsBackDataSchemaAndMarker(t *testing.T) {
	for _, stage := range []string{"rename", "marker"} {
		t.Run(stage, func(t *testing.T) {
			db, original := legacySyncGroups(t)
			trigger := `CREATE TRIGGER fail_sync_name BEFORE UPDATE ON sync_groups WHEN NEW.id = 2 BEGIN SELECT RAISE(ABORT, 'injected rename failure'); END`
			if stage == "marker" {
				trigger = `CREATE TRIGGER fail_sync_name BEFORE INSERT ON schema_migrations WHEN NEW.version = 198 BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`
			}
			if err := db.Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			m := migrationByVersion(t, 198)
			if err := migrateDB(db, []migration{m}); err == nil {
				t.Fatal("injected failure did not abort migration")
			}
			var got []SyncGroup
			if err := db.Order("id").Find(&got).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("partial rename committed: got=%+v want=%+v", got, original)
			}
			if db.Migrator().HasIndex(&SyncGroup{}, "idx_sync_groups_name") {
				t.Fatal("index survived failed transaction")
			}
			var count int64
			if err := db.Model(&SchemaMigration{}).Where("version = 198").Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("failed migration marker=%d err=%v", count, err)
			}
			if err := db.Exec("DROP TRIGGER fail_sync_name").Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatalf("migration cannot recover: %v", err)
			}
		})
	}
}
