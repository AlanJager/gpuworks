package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestLegacyTemplateBranchMarkersUpgradeToMaster(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		`CREATE TABLE models (id INTEGER PRIMARY KEY, operations TEXT)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, model_id INTEGER, template_snapshot TEXT, tokenize_disabled NUMERIC)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT, model_id INTEGER, source TEXT, api_contract TEXT)`,
		`INSERT INTO models(id, operations) VALUES
			(1, '[{"type":"openai.chat"}]'),
			(2, '[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]'),
			(3, '[]')`,
		`INSERT INTO deployments(id, model_id, template_snapshot, tokenize_disabled) VALUES
			(1, 1, '{"api":{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}}', 1),
			(2, 2, '{"template_id":1,"spec_name":"legacy-spec","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}', 0),
			(3, 3, '{"api":{"operations":[{"type":"openai.embeddings"}]}}', 0)`,
		`INSERT INTO inference_specs(id, name, model_id, source, api_contract) VALUES
			(1, 'legacy-spec', 1, 'upstream', '{"operations":[{"type":"openai.embeddings"}]}')`,
	)
	for _, m := range migrations {
		if m.Version > 165 {
			break
		}
		if err := db.Create(&SchemaMigration{Version: m.Version, Name: m.Name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, marker := range []SchemaMigration{
		{Version: 166, Name: "legacy_template_model_operations"},
		{Version: 167, Name: "deployment_tokenize_disabled"},
	} {
		if err := db.Create(&marker).Error; err != nil {
			t.Fatal(err)
		}
	}

	st := &Store{db: db}
	for attempt := 0; attempt < 2; attempt++ {
		if err := st.migrate(); err != nil {
			t.Fatalf("full upgrade %d: %v", attempt+1, err)
		}
	}
	for version, want := range map[int]string{
		165: "deployment_tokenize_disabled",
		166: "notice_reads",
		167: "inference_spec_shm_bytes",
		171: "deleted_nodes",
		172: "legacy_template_model_operations",
	} {
		var marker SchemaMigration
		if err := db.First(&marker, version).Error; err != nil || marker.Name != want {
			t.Fatalf("marker %d = %+v, %v; want %q", version, marker, err, want)
		}
	}
	if !db.Migrator().HasTable("notice_reads") {
		t.Fatal("the old 166 marker did not get master's notice_reads table")
	}
	for _, column := range []string{"shm_bytes", "memlock_unlimited"} {
		if !db.Migrator().HasColumn("inference_specs", column) {
			t.Fatalf("inference_specs.%s is missing after the full upgrade", column)
		}
	}
	var shm int64
	if err := db.Raw(`SELECT shm_bytes FROM inference_specs WHERE id = 1`).Scan(&shm).Error; err != nil || shm != 0 {
		t.Fatalf("legacy spec shm_bytes = %d, %v", shm, err)
	}
	var tokenizeDisabled bool
	if err := db.Raw(`SELECT tokenize_disabled FROM deployments WHERE id = 1`).Scan(&tokenizeDisabled).Error; err != nil || !tokenizeDisabled {
		t.Fatalf("tokenize setting was not preserved: %v, %v", tokenizeDisabled, err)
	}
	operations := modelOperationsColumn(t, db)
	if got, want := operations[1], `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`; got != want {
		t.Fatalf("legacy placeholder after upgrade = %q, want %q", got, want)
	}
	if got, want := operations[2], `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`; got != want {
		t.Fatalf("ambiguous normalized-chat contract changed: %q, want %q", got, want)
	}
	if operations[3] != `[]` {
		t.Fatalf("the data repair rewrote an empty array: %q", operations[3])
	}
}

func TestLegacyTemplateBranchRepairRollsBackAndRetries(t *testing.T) {
	for _, tc := range []struct {
		version      int
		recordedName string
		schema       string
	}{
		{166, "legacy_template_model_operations", ""},
		{167, "deployment_tokenize_disabled", `CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT)`},
	} {
		t.Run(tc.recordedName, func(t *testing.T) {
			statements := []string{}
			if tc.schema != "" {
				statements = append(statements, tc.schema)
			}
			db := openUpgradeCompatibilityDB(t, statements...)
			base := migrationByVersion(t, tc.version)
			if err := db.Create(&SchemaMigration{Version: tc.version, Name: tc.recordedName}).Error; err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected legacy branch repair failure")
			failing := base
			failing.Apply = func(tx *gorm.DB) error {
				if err := base.Apply(tx); err != nil {
					return err
				}
				return injected
			}
			if err := migrateDB(db, []migration{failing}); !errors.Is(err, injected) {
				t.Fatalf("repair error = %v, want %v", err, injected)
			}
			assertLegacyBranchSchema := func(want bool) {
				t.Helper()
				var got bool
				if tc.version == 166 {
					got = db.Migrator().HasTable("notice_reads")
				} else {
					got = db.Migrator().HasColumn("inference_specs", "shm_bytes")
				}
				if got != want {
					t.Fatalf("schema present = %v, want %v", got, want)
				}
			}
			assertLegacyBranchSchema(false)
			var marker SchemaMigration
			if err := db.First(&marker, tc.version).Error; err != nil || marker.Name != tc.recordedName {
				t.Fatalf("marker after rollback = %+v, %v", marker, err)
			}
			if err := migrateDB(db, []migration{base}); err != nil {
				t.Fatalf("retry: %v", err)
			}
			assertLegacyBranchSchema(true)
			if err := db.First(&marker, tc.version).Error; err != nil || marker.Name != base.Name {
				t.Fatalf("marker after retry = %+v, %v", marker, err)
			}
		})
	}
}

func TestUnknownMigrationNameAtLegacyTemplateBranchVersionIsSkipped(t *testing.T) {
	for _, version := range []int{166, 167} {
		t.Run(migrationByVersion(t, version).Name, func(t *testing.T) {
			db := openUpgradeCompatibilityDB(t, `CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT)`)
			m := migrationByVersion(t, version)
			if err := db.Create(&SchemaMigration{Version: version, Name: "unknown_branch_migration"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			if db.Migrator().HasTable("notice_reads") || db.Migrator().HasColumn("inference_specs", "shm_bytes") {
				t.Fatal("an unknown same-version migration was replayed")
			}
			var marker SchemaMigration
			if err := db.First(&marker, version).Error; err != nil || marker.Name != "unknown_branch_migration" {
				t.Fatalf("unknown marker was rewritten: %+v, %v", marker, err)
			}
		})
	}
}
