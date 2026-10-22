package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestDropMonitorRulesSettingMigration(t *testing.T) {
	drop := migrationByVersion(t, 139)
	if drop.Name != "drop_monitor_rules_setting" {
		t.Fatalf("migration 139 = %q", drop.Name)
	}
	db := openAcquisitionMigrationDB(t, "drop-monitor-rules-setting")
	for _, seed := range []Setting{
		{Key: "monitor.rules", Value: `{"thresholds":{"ttft_p95_warn_sec":3},"enabled":{"idle":false}}`},
		{Key: "deployment", Value: `{"start_timeout_sec":1800}`},
	} {
		if err := db.Create(&seed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, []migration{drop}); err != nil {
		t.Fatal(err)
	}
	if _, ok := settingValue(t, db, "monitor.rules"); ok {
		t.Fatal("the monitor.rules setting survived the migration")
	}
	if value, ok := settingValue(t, db, "deployment"); !ok || value != `{"start_timeout_sec":1800}` {
		t.Fatalf("an unrelated setting changed: %q %v", value, ok)
	}
	assertMigrationMarkers(t, db, 139)
	// Running again is a no-op.
	if err := migrateDB(db, []migration{drop}); err != nil {
		t.Fatal(err)
	}

	// A failing delete rolls the marker back with it.
	failing := migration{Version: 139, Name: drop.Name, Apply: func(tx *gorm.DB) error {
		if err := drop.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected")
	}}
	fresh := openAcquisitionMigrationDB(t, "drop-monitor-rules-setting-fail")
	if err := fresh.Create(&Setting{Key: "monitor.rules", Value: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(fresh, []migration{failing}); err == nil {
		t.Fatal("injected failure not reported")
	}
	if _, ok := settingValue(t, fresh, "monitor.rules"); !ok {
		t.Fatal("the delete was not rolled back")
	}
	var marker SchemaMigration
	if err := fresh.Where("version = ?", 139).First(&marker).Error; err == nil {
		t.Fatal("the marker was written for a failed migration")
	}
}
