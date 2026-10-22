package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func legacyCatalogRows(t *testing.T, db *gorm.DB, sourceColumn string) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS inference_specs`,
		`DROP TABLE IF EXISTS inference_models`,
		`CREATE TABLE inference_models (id INTEGER PRIMARY KEY, model_id TEXT NOT NULL, source TEXT NOT NULL, package_digest TEXT)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER NOT NULL, name TEXT NOT NULL, engine TEXT, image TEXT, weight_sources TEXT, gpus TEXT` + sourceColumn + `)`,
		`CREATE UNIQUE INDEX idx_inference_spec_identity ON inference_specs(model_id, name)`,
		`INSERT INTO inference_models(id, model_id, source, package_digest) VALUES(1, 'acme/user', 'user', 'historical-package')`,
		`INSERT INTO inference_specs(id, model_id, name, engine, image, weight_sources, gpus) VALUES(1, 1, 'default', 'vllm', 'image', '[]', '{}')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func assertLegacyCatalogOwnership(t *testing.T, db *gorm.DB) {
	t.Helper()
	var spec InferenceSpecRow
	if err := db.First(&spec, 1).Error; err != nil {
		t.Fatal(err)
	}
	if spec.Source != "user" || spec.PackageDigest != "historical-package" || spec.Name != "default" || spec.Image != "image" {
		t.Fatalf("historical spec lost: %+v", spec)
	}
}

// GOMAXPROCS=4 go test -p 4 ./internal/server/store -run TestLegacyCatalogOwnershipBeforeSourceMigration
func TestLegacyCatalogOwnershipBeforeSourceMigration(t *testing.T) {
	for _, sourceColumn := range []string{"", ", source TEXT, package_digest TEXT"} {
		t.Run(sourceColumn, func(t *testing.T) {
			db := historicalSchemaDB(t, `CREATE TABLE deployments (id INTEGER PRIMARY KEY)`)
			legacyCatalogRows(t, db, sourceColumn)
			for _, version := range []int{69, 72, 164} {
				if err := migrateDB(db, []migration{migrationByVersion(t, version)}); err != nil {
					t.Fatalf("upgrade migration %d: %v", version, err)
				}
			}
			assertLegacyCatalogOwnership(t, db)
		})
	}
}

func TestLegacyCatalogContractMigrationRollsBackAndRetries(t *testing.T) {
	for _, sourceColumn := range []string{"", ", source TEXT, package_digest TEXT"} {
		t.Run(sourceColumn, func(t *testing.T) {
			db := historicalSchemaDB(t)
			legacyCatalogRows(t, db, sourceColumn)
			base := migrationByVersion(t, 69)
			boom := errors.New("after executable catalog migration")
			failing := base
			failing.Apply = func(tx *gorm.DB) error {
				if err := base.Apply(tx); err != nil {
					return err
				}
				return boom
			}
			if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
				t.Fatalf("rollback error=%v", err)
			}
			if db.Migrator().HasColumn(&InferenceSpecRow{}, "Scenario") {
				t.Fatal("new catalog schema survived rollback")
			}
			var markers int64
			if err := db.Model(&SchemaMigration{}).Count(&markers).Error; err != nil || markers != 0 {
				t.Fatalf("rollback markers=%d err=%v", markers, err)
			}
			if sourceColumn == "" {
				if db.Migrator().HasColumn(&InferenceSpecRow{}, "Source") || db.Migrator().HasColumn(&InferenceSpecRow{}, "PackageDigest") {
					t.Fatal("ownership columns survived rollback")
				}
			} else {
				var nulls int64
				if err := db.Raw("SELECT COUNT(*) FROM inference_specs WHERE source IS NULL AND package_digest IS NULL").Scan(&nulls).Error; err != nil || nulls != 1 {
					t.Fatalf("ownership values survived rollback: nulls=%d err=%v", nulls, err)
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := migrateDB(db, []migration{base}); err != nil {
					t.Fatalf("retry %d: %v", attempt, err)
				}
				assertLegacyCatalogOwnership(t, db)
			}
		})
	}
}

func TestLegacyCatalogV68UpgradeToHeadPreservesOwnership(t *testing.T) {
	for _, sourceColumn := range []string{"", ", source TEXT, package_digest TEXT"} {
		t.Run(sourceColumn, func(t *testing.T) {
			db := historicalSchemaDB(t)
			var prior []migration
			for _, m := range migrations {
				if m.Version <= 68 {
					prior = append(prior, m)
				}
			}
			if err := migrateDB(db, prior); err != nil {
				t.Fatal(err)
			}
			// The current Go structs contain future columns. Replace only the
			// catalog with literal v68 wire storage, retaining its migration ledger.
			legacyCatalogRows(t, db, sourceColumn)
			st := &Store{db: db}
			for attempt := 0; attempt < 2; attempt++ {
				if err := st.migrate(); err != nil {
					t.Fatalf("start %d: %v", attempt, err)
				}
				assertLegacyCatalogOwnership(t, db)
				version, err := st.CurrentMigration()
				if err != nil || version != migrations[len(migrations)-1].Version {
					t.Fatalf("final version=%d err=%v", version, err)
				}
			}
		})
	}
}
