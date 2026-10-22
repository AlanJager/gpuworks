package store

import (
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestDeploymentRestartRequestedAtMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO deployments (id, name) VALUES (1, 'old')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 116)

	db := open("restart-requested-at")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "RestartRequestedAt") {
		t.Fatal("restart_requested_at was not added")
	}
	// An existing deployment has no restart in progress.
	var pending int64
	if err := db.Raw("SELECT COUNT(*) FROM deployments WHERE restart_requested_at IS NOT NULL").Scan(&pending).Error; err != nil || pending != 0 {
		t.Fatalf("existing rows with a restart = %d err=%v", pending, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 116).Error; err != nil || marker.Name != "deployment_restart_requested_at" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 116").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("restart-requested-at-rollback")
	want := errors.New("injected restart_requested_at failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "RestartRequestedAt") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 116).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	// The next start applies it cleanly.
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Deployment{}, "RestartRequestedAt") {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}

func TestRollingRestartRequestAndFinish(t *testing.T) {
	st, err := Open("file:store_rolling_restart_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("roll-node", HashToken("roll-token"), "roll")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active", Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []Instance{{UUID: "roll-ok", State: proto.InstRunning}, {UUID: "roll-failed", State: proto.InstFailed}} {
		row.DeploymentID, row.NodeID = dep.ID, node.ID
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	first := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.RequestRollingRestart(dep.ID, first); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(dep.ID)
	if got.RestartRequestedAt == nil || !got.RestartRequestedAt.Equal(first) {
		t.Fatalf("restart_requested_at = %v, want %v", got.RestartRequestedAt, first)
	}
	if inst, _ := st.InstanceByUUID("roll-failed"); inst.State != proto.InstRemoving {
		t.Fatalf("failed attempt = %s, want removing", inst.State)
	}
	if inst, _ := st.InstanceByUUID("roll-ok"); inst.State != proto.InstRunning {
		t.Fatalf("running replica = %s", inst.State)
	}

	// A newer request is not cleared by finishing the older one.
	second := first.Add(time.Second)
	if err := st.RequestRollingRestart(dep.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRollingRestart(dep.ID, first); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.RestartRequestedAt == nil || !got.RestartRequestedAt.Equal(second) {
		t.Fatalf("finishing the older request cleared the newer one: %v", got.RestartRequestedAt)
	}
	read, _ := st.GetDeployment(dep.ID)
	if err := st.FinishRollingRestart(dep.ID, *read.RestartRequestedAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.RestartRequestedAt != nil {
		t.Fatalf("finished restart still recorded: %v", got.RestartRequestedAt)
	}

	// Stopping ends a restart in progress.
	if err := st.RequestRollingRestart(dep.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentState(dep.ID, DeployStopped); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.RestartRequestedAt != nil {
		t.Fatalf("stopped deployment kept its restart: %v", got.RestartRequestedAt)
	}
	// A stopped (or unknown) deployment is not rolled.
	if err := st.RequestRollingRestart(dep.ID, second); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("rolling a stopped deployment: err = %v", err)
	}
	if err := st.RequestRollingRestart(9999, second); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("unknown deployment: err = %v", err)
	}
}

// Restarting at once retires every instance that has not already begun
// leaving, failed attempts included, and ends a rolling restart, in one write;
// a deployment that stopped meanwhile is refused and untouched.
func TestRestartDeploymentNowRetiresEveryInstanceAtOnce(t *testing.T) {
	st, err := Open("file:store_restart_now_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, _ := st.RegisterNode("now-node", HashToken("now-token"), "now")
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active", Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []Instance{
		{UUID: "now-running", State: proto.InstRunning},
		{UUID: "now-failed", State: proto.InstFailed},
		{UUID: "now-draining", State: proto.InstDraining},
		{UUID: "now-stopped", State: proto.InstStopped},
		{UUID: "now-removing", State: proto.InstRemoving},
	} {
		row.DeploymentID, row.NodeID = dep.ID, node.ID
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RequestRollingRestart(dep.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	restarted, err := st.RestartDeploymentNow(dep.ID)
	if err != nil || restarted != 2 {
		// now-failed was already retired by the rolling request.
		t.Fatalf("restarted = %d err=%v, want 2 (running, draining)", restarted, err)
	}
	for uuid, want := range map[string]string{"now-running": proto.InstRemoving, "now-draining": proto.InstRemoving, "now-failed": proto.InstRemoving, "now-stopped": proto.InstStopped} {
		if got, _ := st.InstanceByUUID(uuid); got.State != want {
			t.Errorf("%s = %s, want %s", uuid, got.State, want)
		}
	}
	if got, _ := st.GetDeployment(dep.ID); got.RestartRequestedAt != nil {
		t.Fatalf("restarting at once left the rolling restart: %v", got.RestartRequestedAt)
	}
	if err := st.SetDeploymentState(dep.ID, DeployStopped); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestartDeploymentNow(dep.ID); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("restarting a stopped deployment: err = %v", err)
	}
}

// Starting is conditional on the deployment being stopped: an active one is
// left as it is, a deleting one is never brought back.
func TestStartDeploymentOnlyStartsAStoppedDeployment(t *testing.T) {
	st, err := Open("file:store_start_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, tc := range []struct{ from, want DeploymentState }{
		{DeployStopped, DeployActive},
		{DeployActive, DeployActive},
		{DeployDeleting, DeployDeleting},
	} {
		dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: 1, State: tc.from})
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.StartDeployment(dep.ID)
		if err != nil || got != tc.want {
			t.Fatalf("start from %s = %s %v, want %s", tc.from, got, err, tc.want)
		}
		if stored, _ := st.GetDeployment(dep.ID); stored.State != tc.want {
			t.Fatalf("stored state from %s = %s, want %s", tc.from, stored.State, tc.want)
		}
	}
	if _, err := st.StartDeployment(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment: err = %v", err)
	}
}
