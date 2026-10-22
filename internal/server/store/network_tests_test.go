package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestNetworkTestRunLifecycle(t *testing.T) {
	s := newTestStore(t)
	n, err := s.CreateNetwork(&Network{Name: "lan", Type: "ethernet", Roles: `["storage_sync"]`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LatestNetworkTestRun(n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("latest without runs err = %v", err)
	}
	if _, err := s.StartNetworkTestRun(&NetworkTestRun{NetworkID: 999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("start on missing network err = %v", err)
	}
	run, err := s.StartNetworkTestRun(&NetworkTestRun{NetworkID: n.ID, Scope: "all", NodeCount: 3, Bandwidth: true})
	if err != nil || run.Status != NetworkTestRunning || run.StartedAt.IsZero() {
		t.Fatalf("start = %+v, %v", run, err)
	}
	if _, err := s.StartNetworkTestRun(&NetworkTestRun{NetworkID: n.ID}); !errors.Is(err, ErrNetworkTestRunning) {
		t.Fatalf("concurrent start err = %v", err)
	}
	if err := s.FinishNetworkTestRun(run.ID, `[{"source_uuid":"a"}]`, 6, 5); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestNetworkTestRun(n.ID)
	if err != nil || latest.Status != NetworkTestDone || latest.PassedCount != 5 || latest.PairCount != 6 || latest.FinishedAt == nil || latest.Pairs != `[{"source_uuid":"a"}]` {
		t.Fatalf("latest = %+v, %v", latest, err)
	}

	// Only the newest runs are kept.
	for i := 0; i < networkTestKeep+3; i++ {
		r, err := s.StartNetworkTestRun(&NetworkTestRun{NetworkID: n.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishNetworkTestRun(r.ID, "[]", 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	s.db.Model(&NetworkTestRun{}).Where("network_id = ?", n.ID).Count(&count)
	if count != networkTestKeep {
		t.Fatalf("kept runs = %d, want %d", count, networkTestKeep)
	}

	// A run the server lost by restarting stops blocking new runs.
	lost, err := s.StartNetworkTestRun(&NetworkTestRun{NetworkID: n.ID})
	if err != nil {
		t.Fatal(err)
	}
	s.db.Model(&NetworkTestRun{}).Where("id = ?", lost.ID).Update("started_at", time.Now().UTC().Add(-3*time.Hour))
	if latest, _ := s.LatestNetworkTestRun(n.ID); latest.Status != NetworkTestInterrupted {
		t.Fatalf("stale run status = %s", latest.Status)
	}
	if _, err := s.StartNetworkTestRun(&NetworkTestRun{NetworkID: n.ID}); err != nil {
		t.Fatalf("start after stale run: %v", err)
	}
	var stale NetworkTestRun
	s.db.First(&stale, lost.ID)
	if stale.Status != NetworkTestInterrupted || stale.FinishedAt == nil {
		t.Fatalf("stale row = %+v", stale)
	}

	if err := s.DeleteNetwork(n.ID); err != nil {
		t.Fatal(err)
	}
	s.db.Model(&NetworkTestRun{}).Count(&count)
	if count != 0 {
		t.Fatalf("runs left after deleting the network: %d", count)
	}
}

func TestNetworkTestRunsMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-test-runs-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 97)
	failing := migration{Version: 97, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasTable(&NetworkTestRun{}) {
		t.Fatalf("failed migration left schema behind (err=%v)", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasTable(&NetworkTestRun{}) {
		t.Fatalf("migration err = %v", err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 97).Error; err != nil || marker.Name != "network_test_runs" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestNetworkTestRunKindMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-test-run-kind-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &NetworkTestRun{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&networkTestRunImageColumns{}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"kind", "image"} {
		if err := db.Exec("ALTER TABLE network_test_runs DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByVersion(t, 100)
	failing := migration{Version: 100, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasColumn(&NetworkTestRun{}, "Kind") {
		t.Fatalf("failed migration left a column (err=%v)", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&NetworkTestRun{}, "Kind") || !db.Migrator().HasColumn(&networkTestRunImageColumns{}, "Image") {
		t.Fatalf("migration err = %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestNetworkTestRunPullImageMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-test-run-pull-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &NetworkTestRun{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&networkTestRunImageColumns{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE network_test_runs DROP COLUMN pull_image").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 101)
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&networkTestRunImageColumns{}, "PullImage") {
		t.Fatalf("migration err = %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestDropNCCLNetworkTestsMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:drop-nccl-network-tests?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// The table as migrations 100 and 101 left it, with runs of every kind.
	if err := db.AutoMigrate(&SchemaMigration{}, &NetworkTestRun{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&networkTestRunImageColumns{}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tcp", "nccl_image", "rdma_tool", "nccl_image"} {
		if err := db.Exec("INSERT INTO network_test_runs (network_id, status, kind, image, pull_image) VALUES (1, 'done', ?, 'img', true)", kind).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByVersion(t, 105)
	failing := migration{Version: 105, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	var count int64
	if err := migrateDB(db, []migration{failing}); err == nil {
		t.Fatal("injected failure was swallowed")
	}
	db.Model(&NetworkTestRun{}).Count(&count)
	if count != 4 || !db.Migrator().HasColumn(&networkTestRunImageColumns{}, "Image") {
		t.Fatalf("failed migration left changes: %d runs", count)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	db.Model(&NetworkTestRun{}).Order("id").Pluck("kind", &kinds)
	if !reflect.DeepEqual(kinds, []string{"tcp", "rdma_tool"}) {
		t.Fatalf("kinds left = %v", kinds)
	}
	for _, column := range []string{"Image", "PullImage"} {
		if db.Migrator().HasColumn(&networkTestRunImageColumns{}, column) {
			t.Fatalf("column %s survived", column)
		}
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 105).First(&marker).Error; err != nil || marker.Name != "drop_nccl_network_tests" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestRDMANetworkInferredMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:rdma-network-inferred?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// networks as migration 99 left it, with the HCA glob column.
	if err := db.AutoMigrate(&SchemaMigration{}, &Network{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE networks ADD COLUMN hca text").Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]string{{"ib", "rdma", "ib*", "mlx5_*"}, {"lan", "ethernet", "eth*", ""}} {
		if err := db.Exec("INSERT INTO networks (name, type, interface, hca) VALUES (?, ?, ?, ?)", row[0], row[1], row[2], row[3]).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByVersion(t, 106)
	failing := migration{Version: 106, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil {
		t.Fatal("injected failure was swallowed")
	}
	var ifaces []string
	db.Model(&Network{}).Order("id").Pluck("interface", &ifaces)
	if !db.Migrator().HasColumn("networks", "hca") || !reflect.DeepEqual(ifaces, []string{"ib*", "eth*"}) {
		t.Fatalf("failed migration left changes: %v", ifaces)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	ifaces = nil
	db.Model(&Network{}).Order("id").Pluck("interface", &ifaces)
	if db.Migrator().HasColumn("networks", "hca") || !reflect.DeepEqual(ifaces, []string{"", "eth*"}) {
		t.Fatalf("after migration: hca column kept or interfaces %v", ifaces)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 106).First(&marker).Error; err != nil || marker.Name != "rdma_network_inferred" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestNetworkTestRunPatternMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-test-run-pattern?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &NetworkTestRun{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE network_test_runs DROP COLUMN pattern").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 107)
	failing := migration{Version: 107, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasColumn(&NetworkTestRun{}, "Pattern") {
		t.Fatalf("failed migration left a column (err=%v)", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&NetworkTestRun{}, "Pattern") {
		t.Fatalf("migration err = %v", err)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 107).First(&marker).Error; err != nil || marker.Name != "network_test_run_pattern" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}
