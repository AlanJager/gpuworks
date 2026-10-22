package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// acquisitionMigrations are the settings migrations introduced when deployment
// acquisition became an explicit per-request choice.
func acquisitionMigrations() []migration {
	var out []migration
	for _, m := range migrations {
		if m.Version == 78 || m.Version == 79 {
			out = append(out, m)
		}
	}
	return out
}

func openAcquisitionMigrationDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Setting{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func settingValue(t *testing.T, db *gorm.DB, key string) (string, bool) {
	t.Helper()
	var row Setting
	err := db.Where("key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row.Value, true
}

func networkRewrites(t *testing.T, db *gorm.DB) []proto.URLRewrite {
	t.Helper()
	raw, ok := settingValue(t, db, "network")
	if !ok {
		return nil
	}
	var cfg proto.NetworkConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("network setting %q is not a NetworkConfig: %v", raw, err)
	}
	return cfg.Rewrites
}

func assertMigrationMarkers(t *testing.T, db *gorm.DB, versions ...int) {
	t.Helper()
	for _, version := range versions {
		var marker SchemaMigration
		if err := db.Where("version = ?", version).First(&marker).Error; err != nil {
			t.Fatalf("migration %d marker missing: %v", version, err)
		}
	}
}

func TestAcquisitionMigrationsDropDefaultModelSourceAndFoldModelEndpoint(t *testing.T) {
	db := openAcquisitionMigrationDB(t, "acq-migrate-both")
	for _, seed := range []Setting{
		{Key: "default_model_source", Value: `"modelscope"`},
		{Key: "model_endpoint", Value: `"https://hf-mirror.example/"`},
		{Key: "template_catalog_url", Value: `"https://catalog.example"`},
	} {
		if err := db.Create(&seed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, acquisitionMigrations()); err != nil {
		t.Fatal(err)
	}
	if _, ok := settingValue(t, db, "default_model_source"); ok {
		t.Fatal("default_model_source survived the migration")
	}
	if _, ok := settingValue(t, db, "model_endpoint"); ok {
		t.Fatal("model_endpoint survived the migration")
	}
	rewrites := networkRewrites(t, db)
	if len(rewrites) != 1 || rewrites[0].From != "https://huggingface.co" || rewrites[0].To != "https://hf-mirror.example" {
		t.Fatalf("network rewrites = %#v", rewrites)
	}
	// Unrelated settings must be left alone.
	if value, ok := settingValue(t, db, "template_catalog_url"); !ok || value != `"https://catalog.example"` {
		t.Fatalf("template_catalog_url = %q ok=%v", value, ok)
	}
	assertMigrationMarkers(t, db, 78, 79)
}

func TestAcquisitionMigrationsAreIdempotentAcrossRepeatedStartups(t *testing.T) {
	db := openAcquisitionMigrationDB(t, "acq-migrate-idempotent")
	if err := db.Create(&Setting{Key: "model_endpoint", Value: `"https://hf-mirror.example"`}).Error; err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := migrateDB(db, acquisitionMigrations()); err != nil {
			t.Fatal(err)
		}
	}
	if rewrites := networkRewrites(t, db); len(rewrites) != 1 {
		t.Fatalf("repeated startups changed rewrites: %#v", rewrites)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version IN ?", []int{78, 79}).Count(&markers).Error; err != nil {
		t.Fatal(err)
	}
	if markers != 2 {
		t.Fatalf("migration markers = %d, want 2", markers)
	}
}

func TestModelEndpointMigrationPreservesAnOperatorAuthoredRuleForTheSameUpstream(t *testing.T) {
	db := openAcquisitionMigrationDB(t, "acq-migrate-operator-rule")
	existing, err := json.Marshal(proto.NetworkConfig{
		HTTPProxy: "http://proxy.example:3128",
		Rewrites:  []proto.URLRewrite{{From: "https://huggingface.co", To: "https://operator.example"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []Setting{
		{Key: "model_endpoint", Value: `"https://hf-mirror.example"`},
		{Key: "network", Value: string(existing)},
	} {
		if err := db.Create(&seed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, acquisitionMigrations()); err != nil {
		t.Fatal(err)
	}
	rewrites := networkRewrites(t, db)
	if len(rewrites) != 1 || rewrites[0].To != "https://operator.example" {
		t.Fatalf("operator rule was replaced: %#v", rewrites)
	}
	if _, ok := settingValue(t, db, "model_endpoint"); ok {
		t.Fatal("model_endpoint survived the migration")
	}
	// Unrelated network fields must round-trip through the rewrite.
	raw, _ := settingValue(t, db, "network")
	var cfg proto.NetworkConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPProxy != "http://proxy.example:3128" {
		t.Fatalf("network config lost unrelated fields: %#v", cfg)
	}
}

func TestModelEndpointMigrationAppendsBesideUnrelatedRewriteRules(t *testing.T) {
	db := openAcquisitionMigrationDB(t, "acq-migrate-append")
	existing, err := json.Marshal(proto.NetworkConfig{
		Rewrites: []proto.URLRewrite{{From: "https://modelscope.cn", To: "https://ms-mirror.example"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []Setting{
		{Key: "model_endpoint", Value: `"https://hf-mirror.example"`},
		{Key: "network", Value: string(existing)},
	} {
		if err := db.Create(&seed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, acquisitionMigrations()); err != nil {
		t.Fatal(err)
	}
	rewrites := networkRewrites(t, db)
	if len(rewrites) != 2 || rewrites[0].From != "https://modelscope.cn" || rewrites[1].To != "https://hf-mirror.example" {
		t.Fatalf("rewrites = %#v", rewrites)
	}
}

func TestModelEndpointMigrationHandlesBareStringEmptyAndUnusableEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		wantRewrite  string
	}{
		{"bare string from server startup wiring", "https://hf-mirror.example", "https://hf-mirror.example"},
		{"json string from the settings API", `"https://hf-mirror.example/"`, "https://hf-mirror.example"},
		{"empty value carries nothing over", `""`, ""},
		{"whitespace only", `"   "`, ""},
		{"relative endpoint is unusable", `"hf-mirror.example"`, ""},
		{"scheme without a host is unusable", `"https://"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAcquisitionMigrationDB(t, "acq-migrate-value-"+t.Name())
			if err := db.Create(&Setting{Key: "model_endpoint", Value: tc.stored}).Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, acquisitionMigrations()); err != nil {
				t.Fatalf("migration failed on stored value %q: %v", tc.stored, err)
			}
			// However unusable the value was, the obsolete key must be gone.
			if _, ok := settingValue(t, db, "model_endpoint"); ok {
				t.Fatal("model_endpoint survived the migration")
			}
			rewrites := networkRewrites(t, db)
			if tc.wantRewrite == "" {
				if len(rewrites) != 0 {
					t.Fatalf("unusable endpoint produced rewrites: %#v", rewrites)
				}
				return
			}
			if len(rewrites) != 1 || rewrites[0].To != tc.wantRewrite {
				t.Fatalf("rewrites = %#v, want To=%q", rewrites, tc.wantRewrite)
			}
		})
	}
}

func TestAcquisitionMigrationsSucceedWithNoSettingsAtAll(t *testing.T) {
	db := openAcquisitionMigrationDB(t, "acq-migrate-empty")
	if err := migrateDB(db, acquisitionMigrations()); err != nil {
		t.Fatal(err)
	}
	if _, ok := settingValue(t, db, "network"); ok {
		t.Fatal("migration invented a network setting")
	}
	assertMigrationMarkers(t, db, 78, 79)
}

// A legacy database may reach these migrations before the settings table
// exists. They must no-op rather than abort startup.
func TestAcquisitionMigrationsNoOpWithoutASettingsTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:acq-migrate-no-settings?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, acquisitionMigrations()); err != nil {
		t.Fatalf("migrations aborted without a settings table: %v", err)
	}
	assertMigrationMarkers(t, db, 78, 79)
}

// The marker and the settings change must share one transaction: a failure
// after the rewrite is written must leave neither behind.
func TestModelEndpointMigrationRollsBackSettingAndMarkerTogether(t *testing.T) {
	db := openAcquisitionMigrationDB(t, "acq-migrate-rollback")
	if err := db.Create(&Setting{Key: "model_endpoint", Value: `"https://hf-mirror.example"`}).Error; err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected failure after the settings rewrite")
	err := applyMigration(db, migration{Version: 79, Name: "model_endpoint_to_network_rewrite", Apply: func(tx *gorm.DB) error {
		if err := migrateModelEndpointToNetworkRewrite(tx); err != nil {
			return err
		}
		return injected
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("applyMigration error = %v, want %v", err, injected)
	}
	if _, ok := settingValue(t, db, "model_endpoint"); !ok {
		t.Fatal("model_endpoint deletion was not rolled back")
	}
	if rewrites := networkRewrites(t, db); len(rewrites) != 0 {
		t.Fatalf("network rewrite survived the rollback: %#v", rewrites)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 79).First(&marker).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("marker after rollback = %+v err=%v", marker, err)
	}
	// Re-running after the rollback must still converge.
	if err := migrateDB(db, acquisitionMigrations()); err != nil {
		t.Fatal(err)
	}
	if rewrites := networkRewrites(t, db); len(rewrites) != 1 {
		t.Fatalf("retry after rollback = %#v", rewrites)
	}
}
