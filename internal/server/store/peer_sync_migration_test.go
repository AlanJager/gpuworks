package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestPeerSyncRateLimitMigrationAddsColumnsAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:peer-sync-rate-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &ImageNode{}, &ModelFile{}); err != nil {
		t.Fatal(err)
	}
	for _, tableColumn := range [][2]string{{"image_nodes", "rate_limit_kib"}, {"image_nodes", "last_rate_limit_kib"}, {"model_files", "last_rate_limit"}} {
		if err := db.Exec("ALTER TABLE " + tableColumn[0] + " DROP COLUMN " + tableColumn[1]).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByVersion(t, 95)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		model any
		name  string
	}{{&ImageNode{}, "RateLimitKiB"}, {&ImageNode{}, "LastRateLimitKiB"}, {&ModelFile{}, "LastRateLimit"}} {
		if !db.Migrator().HasColumn(item.model, item.name) {
			t.Fatalf("missing migrated column %T.%s", item.model, item.name)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 95).Error; err != nil || marker.Name != "peer_sync_rate_limits" {
		t.Fatalf("migration marker = %+v, err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}
