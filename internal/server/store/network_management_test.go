package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestNodeManagementRolesOffMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:node-management-roles-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE nodes DROP COLUMN management_roles_off").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 102)
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&Node{}, "ManagementRolesOff") {
		t.Fatalf("migration err = %v", err)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 102).First(&marker).Error; err != nil || marker.Name != "node_management_roles_off" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// A node from before the column carries every role.
	if off := DecodeStringList(Node{}.ManagementRolesOff); len(off) != 0 {
		t.Fatalf("roles off = %v", off)
	}
}

func TestDropNodeDirectAddressMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:drop-direct-address-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE nodes ADD COLUMN direct_address varchar(128)").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 103)
	hasColumn := func() bool {
		var n int64
		_ = db.Raw("SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name = 'direct_address'").Scan(&n).Error
		return n > 0
	}
	if !hasColumn() {
		t.Fatal("fixture has no direct_address column")
	}
	if err := migrateDB(db, []migration{m}); err != nil || hasColumn() {
		t.Fatalf("migration err = %v, column still there = %v", err, hasColumn())
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 103).First(&marker).Error; err != nil || marker.Name != "drop_node_direct_address" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}
