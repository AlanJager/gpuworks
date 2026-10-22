package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// A callable deployment created before the engine stopped appending
// --model/--host/--port keeps launching the argv it always had: the migration
// writes those arguments into its snapshot as template variables, in the old
// order. Everything else in the row and the document survives, a snapshot that
// already places ${API_PORT} and every non-callable row are untouched, a
// document that does not decode stays as it was, and a failure rolls the rows
// and the marker back together.
func TestCallableTemplateArgsMigration(t *testing.T) {
	const (
		legacy  = `{"engine":"callable","extra_args":["--adapter","transformers_adapter"],"env":{"PYTHONPATH":"/opt/gpuworks/runtime"},"entrypoint":["python3","/opt/gpuworks/runtime/gpuworks_serve.py"],"future_key":{"kept":true}}`
		noArgs  = `{"engine":"callable","entrypoint":["python3","/opt/gpuworks/runtime/gpuworks_serve.py"]}`
		placed  = `{"engine":"callable","extra_args":["--listen","${API_HOST}:${API_PORT}"]}`
		viaEnv  = `{"engine":"callable","env":{"PORT":"${API_PORT}"}}`
		broken  = `{"engine":"callable","extra_args":`
		vllmDoc = `{"engine":"vllm","extra_args":["--enforce-eager"]}`
	)
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT, engine TEXT, template_snapshot TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		for id, row := range []struct{ engine, snapshot string }{
			{"callable", legacy}, {"callable", noArgs}, {"callable", placed}, {"callable", viaEnv},
			{"callable", broken}, {"vllm", vllmDoc}, {"callable", ""},
		} {
			if err := db.Exec("INSERT INTO deployments (id, name, engine, template_snapshot) VALUES (?, ?, ?, ?)", id+1, "d", row.engine, row.snapshot).Error; err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	snapshot := func(db *gorm.DB, id int) string {
		var value string
		if err := db.Raw("SELECT template_snapshot FROM deployments WHERE id = ?", id).Scan(&value).Error; err != nil {
			t.Fatal(err)
		}
		return value
	}
	decode := func(document string) map[string]any {
		var out map[string]any
		if err := json.Unmarshal([]byte(document), &out); err != nil {
			t.Fatalf("decode %s: %v", document, err)
		}
		return out
	}
	serve := []any{"--model", "${MODEL_PATH}", "--host", "${API_HOST}", "--port", "${API_PORT}"}
	m := migrationByName(t, "callable_template_args")

	db := open("callable-template-args")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	got := decode(snapshot(db, 1))
	if want := append(append([]any(nil), serve...), "--adapter", "transformers_adapter"); !reflect.DeepEqual(got["extra_args"], want) {
		t.Fatalf("legacy extra_args = %v, want %v", got["extra_args"], want)
	}
	wantRest := decode(legacy)
	delete(wantRest, "extra_args")
	delete(got, "extra_args")
	if !reflect.DeepEqual(got, wantRest) {
		t.Fatalf("other keys changed: %v want %v", got, wantRest)
	}
	if got := decode(snapshot(db, 2))["extra_args"]; !reflect.DeepEqual(got, serve) {
		t.Fatalf("snapshot without args = %v, want %v", got, serve)
	}
	for id, want := range map[int]string{3: placed, 4: viaEnv, 5: broken, 6: vllmDoc, 7: ""} {
		if got := snapshot(db, id); got != want {
			t.Fatalf("row %d changed: %s want %s", id, got, want)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, m.Version).Error; err != nil || marker.Name != "callable_template_args" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// Running the transform again finds every callable snapshot already placing
	// the port and changes nothing.
	once := snapshot(db, 1)
	if err := db.Transaction(m.Apply); err != nil {
		t.Fatal(err)
	}
	if again := snapshot(db, 1); again != once {
		t.Fatalf("not idempotent: %s then %s", once, again)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("marker skip: %v", err)
	}

	rollback := open("callable-template-args-rollback")
	want := errors.New("injected callable failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if got := snapshot(rollback, 1); got != legacy {
		t.Fatalf("failed migration kept its rewrite: %s", got)
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if got := decode(snapshot(rollback, 1))["extra_args"]; len(got.([]any)) != len(serve)+2 {
		t.Fatalf("restart after rollback did not migrate: %v", got)
	}
}
