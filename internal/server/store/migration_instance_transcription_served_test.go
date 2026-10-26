package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestInstanceTranscriptionServedMigration(t *testing.T) {
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
	m := migrationByVersion(t, 174)

	db := open("instance-openapi-paths")
	if err := db.Exec("INSERT INTO instances (uuid) VALUES ('legacy')").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Instance{}, "TranscriptionServed") {
		t.Fatal("transcription_served was not added")
	}
	var legacy Instance
	if err := db.Where("uuid = ?", "legacy").First(&legacy).Error; err != nil || legacy.TranscriptionServed != nil {
		t.Fatalf("legacy transcription observation = %v, err=%v; want unknown", legacy.TranscriptionServed, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 174).Error; err != nil || marker.Name != "instance_transcription_served" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 174").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("instance-openapi-paths-rollback")
	want := errors.New("injected transcription_served failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Instance{}, "TranscriptionServed") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 174).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Instance{}, "TranscriptionServed") {
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

func TestTranscriptionObservationRemainsWhenALaterReportOmitsIt(t *testing.T) {
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
	// Nil reports preserve unknown, an empty list is known absence, and an
	// advertised route is known presence. A later ordinary status preserves
	// each observed value rather than reverting to unknown or false.
	for _, tc := range []struct {
		name  string
		paths *[]string
		want  *bool
	}{
		{name: "unknown"},
		{name: "empty", paths: ptrPaths([]string{}), want: ptrBool(false)},
		{name: "absent", paths: ptrPaths([]string{"/v1/chat/completions", "/v1/models"}), want: ptrBool(false)},
		{name: "served", paths: ptrPaths([]string{"/v1/audio/transcriptions"}), want: ptrBool(true)},
		{name: "removed", paths: ptrPaths([]string{}), want: ptrBool(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, OpenAPIPaths: tc.paths}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, Message: "still up"}); err != nil {
				t.Fatal(err)
			}
			got, err := st.InstanceByUUID(inst.UUID)
			if err != nil {
				t.Fatal(err)
			}
			if (got.TranscriptionServed == nil) != (tc.want == nil) || tc.want != nil && *got.TranscriptionServed != *tc.want {
				t.Fatalf("transcription served = %v, want %v", got.TranscriptionServed, tc.want)
			}
		})
	}
}

func ptrPaths(paths []string) *[]string { return &paths }
func ptrBool(value bool) *bool          { return &value }
