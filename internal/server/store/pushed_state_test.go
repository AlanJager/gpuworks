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

func dropPushedStateTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, m := range pstate.Models() {
		if err := db.Migrator().DropTable(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE name = 'pushed_state_tables'").Error; err != nil {
		t.Fatal(err)
	}
}

func TestPushedStateMigrationCreatesTablesOnceAndRollsBackWhole(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	dropPushedStateTables(t, s.db)
	m := migrationByName(t, "pushed_state_tables")

	boom := errors.New("after the tables")
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
	for _, model := range pstate.Models() {
		if s.db.Migrator().HasTable(model) {
			t.Fatalf("rolled back migration left table %T", model)
		}
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
	cols, err := s.db.Migrator().ColumnTypes(&pstate.AvailabilityStep{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cols {
		names = append(names, c.Name())
	}
	want := []string{"owner", "feature", "step", "at_ms", "access", "link", "notice_code"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("pstate_availability_step columns = %v, want %v", names, want)
	}
	for _, model := range pstate.Models() {
		if !s.db.Migrator().HasTable(model) {
			t.Fatalf("table of %T missing", model)
		}
	}
}

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

func availabilityWrite(gen int64, features ...string) pstate.TableWrite {
	w := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_authority", Kind: extapi.KindAvailability, Schema: 1, Generation: gen, Digest: "sha256:x", PushedAtMS: 1000 * gen}}
	w.Insert.Availability = []pstate.AvailabilityTable{{Owner: "acme_authority", DownGraceS: 600}}
	for _, f := range features {
		w.Insert.AvailabilityRows = append(w.Insert.AvailabilityRows, pstate.AvailabilityRow{Owner: "acme_authority", Feature: f, DownAccess: "read_only"})
		w.Insert.AvailabilitySteps = append(w.Insert.AvailabilitySteps,
			pstate.AvailabilityStep{Owner: "acme_authority", Feature: f, Step: 0, Access: "full"},
			pstate.AvailabilityStep{Owner: "acme_authority", Feature: f, Step: 1, AtMS: 5000, Access: "read_only", NoticeCode: "ACME_EXPIRED"})
		w.Insert.AvailabilityLimits = append(w.Insert.AvailabilityLimits, pstate.AvailabilityLimit{Owner: "acme_authority", Feature: f, Step: 0, Dim: "gpus", Max: 4})
		w.Insert.Params = append(w.Insert.Params, pstate.NoticeParam{Owner: "acme_authority", Kind: extapi.KindAvailability, Entry: f, Slot: pstate.StepSlot(1), Key: "days", Int: i64(3)})
	}
	return w
}

func TestPushedStateWriteIsOneTransactionAndRefusesStaleGenerations(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	first := availabilityWrite(2, "acme.backup", "acme.reports")
	holds := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Schema: 1, Generation: 1, Digest: "sha256:y"}}
	holds.Insert.NodeHolds = []pstate.NodeHold{{Owner: "acme_ops", NodeID: 9, Code: "ACME_MAINTENANCE"}, {Owner: "acme_ops", NodeID: 12, Code: "ACME_QUARANTINE"}}
	holds.Insert.Params = []pstate.NoticeParam{{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Slot: pstate.SlotNode, Key: "ticket", Str: str("OPS-1")}}
	res, err := s.WritePushedState(ctx, &pstate.Batch{
		Tables:    []pstate.TableWrite{first, holds},
		Down:      []pstate.Down{{Owner: "acme_authority", SinceMS: 77}},
		Overrides: []pstate.Override{{Owner: "acme_ops", Kind: extapi.KindNodeHolds, EntryKey: "9", Generation: 1, Action: pstate.OverrideDelete, ByUser: 1, AtMS: 5, Reason: "reboot done"}},
		Clock:     &pstate.Clock{HWMMS: 123},
		Markers:   []pstate.Marker{{Name: "pstate.availability", Version: 1, WrittenBy: "v26.12.0"}},
	})
	if err != nil || len(res.Stale) != 0 || len(res.OverrideIDs) != 1 || res.OverrideIDs[0] == 0 {
		t.Fatalf("write = %+v, %v", res, err)
	}

	// The same generation again is stale and changes nothing; a newer one
	// replaces only the entries it names.
	res, err = s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{availabilityWrite(2, "acme.other")}})
	if err != nil || !reflect.DeepEqual(res.Stale, []pstate.OwnerKind{{Owner: "acme_authority", Kind: extapi.KindAvailability}}) {
		t.Fatalf("stale write = %+v, %v", res, err)
	}
	next := availabilityWrite(3)
	next.DeleteEntries = []string{"acme.reports"}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{next}, Up: []string{"acme_authority"}}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.LoadPushedState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Owners) != 2 || snap.Owners[0].Generation != 3 || snap.Owners[1].Owner != "acme_ops" {
		t.Fatalf("owners = %+v", snap.Owners)
	}
	if len(snap.Rows.AvailabilityRows) != 1 || snap.Rows.AvailabilityRows[0].Feature != "acme.backup" {
		t.Fatalf("rows = %+v", snap.Rows.AvailabilityRows)
	}
	if len(snap.Rows.AvailabilitySteps) != 2 || len(snap.Rows.AvailabilityLimits) != 1 || len(snap.Rows.Availability) != 1 {
		t.Fatalf("steps/limits/header = %+v %+v %+v", snap.Rows.AvailabilitySteps, snap.Rows.AvailabilityLimits, snap.Rows.Availability)
	}
	if len(snap.Down) != 0 || snap.Clock == nil || snap.Clock.HWMMS != 123 || len(snap.Markers) != 1 || len(snap.Overrides) != 1 {
		t.Fatalf("down=%+v clock=%+v markers=%+v overrides=%+v", snap.Down, snap.Clock, snap.Markers, snap.Overrides)
	}
	if p := snap.Rows.Params; len(p) != 2 || *p[0].Int != 3 || *p[1].Str != "OPS-1" {
		t.Fatalf("params = %+v", p)
	}

	// A node hold diff deletes by node id, with the node's parameters.
	diff := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Schema: 1, Generation: 2, Digest: "sha256:z"}, DeleteEntries: []string{"12"}}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{diff}}); err != nil {
		t.Fatal(err)
	}
	if snap, err = s.LoadPushedState(ctx); err != nil {
		t.Fatal(err)
	}
	if len(snap.Rows.NodeHolds) != 1 || snap.Rows.NodeHolds[0].NodeID != 9 || len(snap.Rows.Params) != 1 {
		t.Fatalf("after diff: holds=%+v params=%+v", snap.Rows.NodeHolds, snap.Rows.Params)
	}

	// Uninstall deletes every row of the owner, overrides included; the
	// clock and markers stay.
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Uninstall: []string{"acme_ops"}}); err != nil {
		t.Fatal(err)
	}
	if snap, err = s.LoadPushedState(ctx); err != nil {
		t.Fatal(err)
	}
	if len(snap.Rows.NodeHolds) != 0 || len(snap.Overrides) != 0 || len(snap.Owners) != 1 || snap.Clock == nil || len(snap.Markers) != 1 {
		t.Fatalf("after uninstall: %+v", snap)
	}
}

func TestPushedStateWriteFailureRollsBackEveryPart(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bad := availabilityWrite(1, "acme.backup")
	// A row of another owner breaks the table write's own rule only after
	// the down record and the clock were written: a step whose access is
	// NULL fails its NOT NULL constraint.
	bad.Insert.NodeHolds = []pstate.NodeHold{{Owner: "acme_authority", NodeID: 1, Code: "X"}}
	if err := s.db.Exec("CREATE TRIGGER pstate_test_fail BEFORE INSERT ON pstate_node_hold BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	_, err := s.WritePushedState(ctx, &pstate.Batch{
		Down:   []pstate.Down{{Owner: "acme_authority", SinceMS: 1}},
		Clock:  &pstate.Clock{HWMMS: 9},
		Tables: []pstate.TableWrite{bad},
	})
	if err == nil {
		t.Fatal("the injected failure did not fail the batch")
	}
	snap, err := s.LoadPushedState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Owners) != 0 || len(snap.Rows.AvailabilitySteps) != 0 || len(snap.Down) != 0 || snap.Clock != nil {
		t.Fatalf("failed batch left rows: %+v", snap)
	}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{{Record: pstate.Owner{Owner: "x", Kind: "nope", Generation: 1}}}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestPushedStateSurvivesReopeningAFileDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WritePushedState(context.Background(), &pstate.Batch{Tables: []pstate.TableWrite{availabilityWrite(5, "acme.backup")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	snap, err := s.LoadPushedState(context.Background())
	if err != nil || len(snap.Owners) != 1 || snap.Owners[0].Generation != 5 {
		t.Fatalf("reopened = %+v, %v", snap.Owners, err)
	}
}
