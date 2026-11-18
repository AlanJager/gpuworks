package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A database whose pushed state tables predate the keep table gains it
// once, with its marker, and a failure after the table rolls both back.
func TestCapacityKeepMigrationCreatesTheTableOnceAndRollsBackWhole(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	m := migrationByName(t, "pushed_state_capacity_keep")
	if m.Version <= migrationByName(t, "pushed_state_tables").Version {
		t.Fatalf("version %d is not after the pushed state tables", m.Version)
	}
	if err := s.db.Migrator().DropTable(&pstate.CapacityKeep{}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}

	boom := errors.New("after the table")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("failed apply = %v", err)
	}
	if s.db.Migrator().HasTable(&pstate.CapacityKeep{}) {
		t.Fatal("rolled back migration left the keep table")
	}
	var markers int64
	s.db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers)
	if markers != 0 {
		t.Fatal("rolled back migration left its marker")
	}

	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	s.db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers)
	if markers != 1 {
		t.Fatalf("markers = %d, want 1", markers)
	}
	cols, err := s.db.Migrator().ColumnTypes(&pstate.CapacityKeep{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cols {
		pk, _ := c.PrimaryKey()
		if !pk {
			t.Fatalf("column %s is not part of the primary key", c.Name())
		}
		names = append(names, c.Name())
	}
	if want := []string{"owner", "node_id"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("pstate_fleet_capacity_keep columns = %v, want %v", names, want)
	}
	if err := s.db.Create(&[]pstate.CapacityKeep{{Owner: "acme_authority", NodeID: 3}, {Owner: "acme_authority", NodeID: 3}}).Error; err == nil {
		t.Fatal("a node kept twice by one owner was stored")
	}
}

func capacityWrite(gen int64, keep ...int64) pstate.TableWrite {
	const owner = "acme_authority"
	w := pstate.TableWrite{Record: pstate.Owner{Owner: owner, Kind: extapi.KindFleetCapacity, Schema: 1, Generation: gen, Digest: "sha256:c"}, DeleteAll: true}
	w.Insert.FleetCapacity = []pstate.FleetCapacity{{Owner: owner, Select: extapi.SelectEnrollmentOrder, RefuseNodeCode: "ACME_NODE_LIMIT", RefuseCardCode: "ACME_CARD_LIMIT", HoldCode: "ACME_OVER_CAPACITY"}}
	w.Insert.CapacitySteps = []pstate.CapacityStep{{Owner: owner, Step: 0, MaxNodesWithCards: i64(2)}}
	for _, id := range keep {
		w.Insert.CapacityKeep = append(w.Insert.CapacityKeep, pstate.CapacityKeep{Owner: owner, NodeID: id})
	}
	return w
}

func keptNodes(t *testing.T, s *Store) []int64 {
	t.Helper()
	snap, err := s.LoadPushedState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, k := range snap.Rows.CapacityKeep {
		out = append(out, k.NodeID)
	}
	return out
}

// The kept nodes are replaced with their table, survive a restart, and go
// with their owner.
func TestCapacityKeepIsWrittenWithItsTableAndSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{capacityWrite(1, 3, 7, 12)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{capacityWrite(2, 7, 9)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path, bus.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := keptNodes(t, s); !reflect.DeepEqual(got, []int64{7, 9}) {
		t.Fatalf("kept after reopen = %v", got)
	}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{capacityWrite(3)}}); err != nil {
		t.Fatal(err)
	}
	if got := keptNodes(t, s); got != nil {
		t.Fatalf("a table without keep left %v", got)
	}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{capacityWrite(4, 5)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Uninstall: []string{"acme_authority"}}); err != nil {
		t.Fatal(err)
	}
	if got := keptNodes(t, s); got != nil {
		t.Fatalf("uninstall left %v", got)
	}
}
