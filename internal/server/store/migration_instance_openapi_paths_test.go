package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestInstanceOpenAPIPathsMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT)`).Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 173)

	db := open("instance-openapi-paths")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Instance{}, "OpenAPIPaths") {
		t.Fatal("openapi_paths was not added")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 173).Error; err != nil || marker.Name != "instance_openapi_paths" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 173").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("instance-openapi-paths-rollback")
	want := errors.New("injected openapi_paths failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Instance{}, "OpenAPIPaths") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 173).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Instance{}, "OpenAPIPaths") {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	empty, err := gorm.Open(sqlite.Open("file:instance-openapi-paths-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&Instance{}) {
		t.Fatalf("without instances: err=%v", err)
	}
}

func TestOpenAPIPathsRemainWhenALaterReportOmitsThem(t *testing.T) {
	st, err := Open("file:openapi-paths-keep?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("openapi-node", HashToken("token"), "openapi-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "acme/asr", Revision: "main", Name: "asr"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "asr", State: "active", Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "asr", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/v1/chat/completions", "/v1/models"}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, OpenAPIPaths: &paths}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, Message: "still up"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenAPIPaths != `["/v1/chat/completions","/v1/models"]` {
		t.Fatalf("paths = %s", got.OpenAPIPaths)
	}
}
