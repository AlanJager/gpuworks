package store

import (
	"encoding/json"
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

var readsMigration = migration{Version: 127, Name: "instance_model_files", Apply: migrateInstanceModelFiles}

// beforeReads is a database as migration 126 left it: instances whose desired
// state mounts a node's local file, a shared storage's file under the node's
// mount point, and a template component; one stopped instance; one mount no
// row explains.
func beforeReads(t *testing.T) (*storageFixture, map[string]uint) {
	t.Helper()
	f := newStorageFixture(t)
	st := f.st
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
	component, err := st.UpsertModel(&Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B-draft", Name: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]uint{}
	for name, row := range map[string]*ModelFile{
		"local":  {ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/opt/gpuworks/models/q", State: proto.MFPresent},
		"shared": {ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent},
		"draft":  {ModelID: component.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/opt/gpuworks/models/draft", State: proto.MFPresent},
	} {
		created, err := st.UpsertModelFile(row)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = created.ID
	}
	dep, err := st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", ModelID: f.model.ID, NodeID: f.n1.ID})
	if err != nil {
		t.Fatal(err)
	}
	spec := func(model string, components ...string) string {
		s := proto.InstanceDesiredState{ModelPath: model}
		for _, c := range components {
			s.Mounts = append(s.Mounts, proto.MountSpec{Source: c, Target: "/draft", Purpose: "model_component:draft"})
		}
		s.Mounts = append(s.Mounts, proto.MountSpec{Source: "/dev/shm", Target: "/dev/shm"})
		b, _ := json.Marshal(s)
		return string(b)
	}
	for _, inst := range []Instance{
		{UUID: "on-local", State: proto.InstRunning, DesiredStateJSON: spec("/opt/gpuworks/models/q", "/opt/gpuworks/models/draft")},
		{UUID: "on-nfs", State: proto.InstStarting, DesiredStateJSON: spec("/mnt/gpuworks/model-archive/Qwen/Qwen3-8B")},
		{UUID: "stopped", State: proto.InstStopped, DesiredStateJSON: spec("/opt/gpuworks/models/q")},
		{UUID: "gone", State: proto.InstRunning, DesiredStateJSON: spec("/opt/gpuworks/models/removed")},
	} {
		inst := inst
		inst.DeploymentID, inst.NodeID = dep.ID, f.n1.ID
		if err := st.DB().Create(&inst).Error; err != nil {
			t.Fatal(err)
		}
		ids[inst.UUID] = inst.ID
	}
	// Back to before the migration.
	if err := st.DB().Migrator().DropTable(&InstanceModelFile{}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Where("version = ?", 127).Delete(&SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	return f, ids
}

func readsOf(t *testing.T, db *gorm.DB, instanceID uint) map[uint]string {
	t.Helper()
	var rows []InstanceModelFile
	if err := db.Where("instance_id = ?", instanceID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]string{}
	for _, r := range rows {
		out[r.ModelFileID] = r.HostPath
	}
	return out
}

func TestInstanceModelFilesMigrationRecordsWhatEachInstanceMounts(t *testing.T) {
	f, ids := beforeReads(t)
	db := f.st.DB()
	if err := migrateDB(db, []migration{readsMigration}); err != nil {
		t.Fatal(err)
	}
	if got := readsOf(t, db, ids["on-local"]); len(got) != 2 || got[ids["local"]] != "/opt/gpuworks/models/q" || got[ids["draft"]] != "/opt/gpuworks/models/draft" {
		t.Fatalf("on-local reads = %v", got)
	}
	if got := readsOf(t, db, ids["on-nfs"]); len(got) != 1 || got[ids["shared"]] != "/mnt/gpuworks/model-archive/Qwen/Qwen3-8B" {
		t.Fatalf("on-nfs reads = %v", got)
	}
	if got := readsOf(t, db, ids["stopped"]); len(got) != 0 {
		t.Fatalf("a stopped instance got reads: %v", got)
	}
	if got := readsOf(t, db, ids["gone"]); len(got) != 0 {
		t.Fatalf("a mount no row explains was recorded: %v", got)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 127).First(&marker).Error; err != nil || marker.Name != "instance_model_files" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// Run again (no marker, as after a restore): nothing is recorded twice.
	if err := db.Transaction(migrateInstanceModelFiles); err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := db.Model(&InstanceModelFile{}).Count(&total).Error; err != nil || total != 3 {
		t.Fatalf("records after a second run = %d err=%v", total, err)
	}
	// What the records say is what "in use" reads now.
	if err := f.st.DeleteStorage(f.nfs.ID); !errors.Is(err, ErrStorageInUse) {
		t.Fatalf("delete a storage an instance reads = %v", err)
	}
}

func TestInstanceModelFilesMigrationRollsBackWithItsMarker(t *testing.T) {
	f, _ := beforeReads(t)
	db := f.st.DB()
	boom := errors.New("injected")
	failing := migration{Version: 127, Name: "instance_model_files", Apply: func(tx *gorm.DB) error {
		if err := migrateInstanceModelFiles(tx); err != nil {
			return err
		}
		return boom
	}}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if db.Migrator().HasTable(&InstanceModelFile{}) {
		var rows int64
		db.Model(&InstanceModelFile{}).Count(&rows)
		if rows != 0 {
			t.Fatalf("records after a failed migration = %d", rows)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 127).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers = %d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{readsMigration}); err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := db.Model(&InstanceModelFile{}).Count(&total).Error; err != nil || total != 3 {
		t.Fatalf("records after the rerun = %d err=%v", total, err)
	}
}
