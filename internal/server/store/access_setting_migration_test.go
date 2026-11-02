package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestDropAccessSettingMigration(t *testing.T) {
	var drop migration
	for _, m := range migrations {
		if m.Version == 104 {
			drop = m
		}
	}
	if drop.Name != "drop_access_setting" {
		t.Fatalf("migration 104 = %q", drop.Name)
	}
	db := openAcquisitionMigrationDB(t, "drop-access-setting")
	for _, seed := range []Setting{
		{Key: "access", Value: `{"console_url":"http://47.103.88.152:8808","node_server_url":"http://192.0.2.156:8800"}`},
		{Key: "network", Value: `{"http_proxy":"http://proxy:3128"}`},
	} {
		if err := db.Create(&seed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, []migration{drop}); err != nil {
		t.Fatal(err)
	}
	if _, ok := settingValue(t, db, "access"); ok {
		t.Fatal("the access setting survived the migration")
	}
	if value, ok := settingValue(t, db, "network"); !ok || value != `{"http_proxy":"http://proxy:3128"}` {
		t.Fatalf("an unrelated setting changed: %q %v", value, ok)
	}
	assertMigrationMarkers(t, db, 104)
	// Running again is a no-op.
	if err := migrateDB(db, []migration{drop}); err != nil {
		t.Fatal(err)
	}

	// A failing delete rolls the marker back with it.
	failing := migration{Version: 104, Name: drop.Name, Apply: func(tx *gorm.DB) error {
		if err := drop.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected")
	}}
	fresh := openAcquisitionMigrationDB(t, "drop-access-setting-fail")
	if err := fresh.Create(&Setting{Key: "access", Value: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(fresh, []migration{failing}); err == nil {
		t.Fatal("injected failure not reported")
	}
	if _, ok := settingValue(t, fresh, "access"); !ok {
		t.Fatal("the delete was not rolled back")
	}
	var marker SchemaMigration
	if err := fresh.Where("version = ?", 104).First(&marker).Error; err == nil {
		t.Fatal("the marker was written for a failed migration")
	}
}
