package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// openBefore192 is a current database rolled back to before migration 192:
// instances without l1_host_buffer_held, one instance row already there.
func openBefore192(t *testing.T) *gorm.DB {
	t.Helper()
	s := newTestStore(t)
	if err := dropColumns(s.db, "instances", "l1_host_buffer_held"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = 192").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec(`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level) VALUES (71, 'before-192', 7, 'running', 0)`).Error; err != nil {
		t.Fatal(err)
	}
	return s.db
}

func TestInstanceL1HostBufferMigration(t *testing.T) {
	m := migrationByName(t, "instance_l1_host_buffer")
	if m.Version != 192 {
		t.Fatalf("version = %d, want 192", m.Version)
	}
	ms := []migration{m}
	check := func(db *gorm.DB, step string) {
		t.Helper()
		if ok, _ := tableHasColumn(db, "instances", "l1_host_buffer_held"); !ok {
			t.Fatalf("%s: instances.l1_host_buffer_held missing", step)
		}
		// A row from before is not known to hold a buffer.
		var held bool
		if err := db.Raw("SELECT l1_host_buffer_held FROM instances WHERE id = 71").Scan(&held).Error; err != nil || held {
			t.Fatalf("%s: existing row held = %v err=%v, want false", step, held, err)
		}
	}

	db := openBefore192(t)
	if ok, _ := tableHasColumn(db, "instances", "l1_host_buffer_held"); ok {
		t.Fatal("fixture still has the column")
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	check(db, "migrated")
	var marker SchemaMigration
	if err := db.First(&marker, 192).Error; err != nil || marker.Name != "instance_l1_host_buffer" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 192").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}
	check(db, "rerun")

	t.Run("rollback", func(t *testing.T) { checkL1HostBufferRollback(t, m, ms, check) })
}

func checkL1HostBufferRollback(t *testing.T, m migration, ms []migration, check func(*gorm.DB, string)) {
	rollback := openBefore192(t)
	boom := errors.New("injected failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if ok, _ := tableHasColumn(rollback, "instances", "l1_host_buffer_held"); ok {
		t.Fatal("the failed migration left instances.l1_host_buffer_held behind")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 192").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, ms); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	check(rollback, "after the restart")
}

// The agent's report is the only source of the fact, and only a report of a
// new process clears it (planning.L1HostMemoryRefusal reads it).
func TestInstanceStatusKeepsTheL1HostBufferUntilANewProcess(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Instance{UUID: "buf-1", DeploymentID: 1, State: proto.InstStarting}).Error; err != nil {
		t.Fatal(err)
	}
	read := func() bool {
		t.Helper()
		in, err := s.InstanceByUUID("buf-1")
		if err != nil {
			t.Fatal(err)
		}
		return in.L1HostBufferHeld
	}
	for _, step := range []struct {
		name string
		st   proto.InstanceStatus
		want bool
	}{
		{"running before its prewarm", proto.InstanceStatus{State: proto.InstRunning}, false},
		{"a held flag on an optimizing report is ignored", proto.InstanceStatus{State: proto.InstOptimizing, L1HostBufferHeld: true}, false},
		{"running after the prewarm", proto.InstanceStatus{State: proto.InstRunning, L1HostBufferHeld: true}, true},
		{"an L1 sleep result", proto.InstanceStatus{State: proto.InstRunning, SleepLevel: 1, L1HostBufferHeld: true}, true},
		{"an adopting agent that does not know", proto.InstanceStatus{State: proto.InstRunning}, true},
		{"a report with no state (an observation)", proto.InstanceStatus{}, true},
		{"a new process pulls", proto.InstanceStatus{State: proto.InstPulling}, false},
		{"its prewarm again", proto.InstanceStatus{State: proto.InstRunning, L1HostBufferHeld: true}, true},
		{"a new process is pending", proto.InstanceStatus{State: proto.InstPending}, false},
		{"its prewarm", proto.InstanceStatus{State: proto.InstRunning, L1HostBufferHeld: true}, true},
		{"the process failed", proto.InstanceStatus{State: proto.InstFailed}, true},
	} {
		st := step.st
		st.UUID = "buf-1"
		if err := s.UpdateInstanceStatus(st); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := read(); got != step.want {
			t.Fatalf("%s: held = %v, want %v", step.name, got, step.want)
		}
	}
}
