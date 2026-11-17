package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

var pluginTrustTables = []string{"plugin_trust_roots", "plugin_trust_revocations", "plugin_trust_serials", "plugin_floors", "plugin_markers"}

func dropPluginTrustTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range pluginTrustTables {
		if err := db.Migrator().DropTable(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 222").Error; err != nil {
		t.Fatal(err)
	}
}

func TestPluginTrustMigration(t *testing.T) {
	s := newTestStore(t)
	m := migrationByName(t, "plugin_trust")
	if m.Version != 222 {
		t.Fatalf("plugin_trust migration version = %d, want 222", m.Version)
	}
	for _, table := range pluginTrustTables {
		if !s.db.Migrator().HasTable(table) {
			t.Fatalf("migrated store lacks %s", table)
		}
	}
	if !s.db.Migrator().HasIndex(&PluginRevocation{}, "idx_plugin_revocations_entry") {
		t.Fatal("revocation entries are not unique")
	}

	dropPluginTrustTables(t, s.db)
	boom := errors.New("after the tables")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("failed migration = %v", err)
	}
	for _, table := range pluginTrustTables {
		if s.db.Migrator().HasTable(table) {
			t.Fatalf("failed migration kept %s", table)
		}
	}
	var markers int64
	s.db.Model(&SchemaMigration{}).Where("version = 222").Count(&markers)
	if markers != 0 {
		t.Fatal("failed migration kept its marker")
	}
	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	s.db.Model(&SchemaMigration{}).Where("version = 222 AND name = ?", "plugin_trust").Count(&markers)
	if markers != 1 {
		t.Fatalf("markers = %d after two runs", markers)
	}
	// Rows written before a repeated run survive it.
	if _, err := s.RaisePluginFloor("acme_x", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(s.db); err != nil {
		t.Fatal(err)
	}
	if floors, _ := s.PluginFloors(); floors["acme_x"] != "1.2.0" {
		t.Fatalf("floors after a repeated apply = %v", floors)
	}
}

func rootKey(id string, b byte) extapi.PublicKey {
	key := make([]byte, 32)
	key[0] = b
	return extapi.PublicKey{KeyID: id, Alg: extapi.AlgEd25519, Key: key}
}

func TestApplyPluginTrustRefusesReplayAndAppliesAtomically(t *testing.T) {
	s := newTestStore(t)
	u := extapi.TrustUpdate{
		Serial:      3,
		AddRoots:    []extapi.PublicKey{rootKey("gwroot-3", 3)},
		RetireRoots: []string{"gwroot-1"},
		Revoke:      []extapi.Revocation{{Publisher: "acme", KeyID: "acme-old"}, {Publisher: "acme"}},
		Floors:      []extapi.Floor{{ID: "acme_lic", MinVersion: "2.0.0"}},
	}
	u.Revoke[1].Manifest = extapi.Digest("sha256:aa")
	applied, highest, err := s.ApplyPluginTrust("trust_update", "gwroot", u)
	if err != nil || !applied || highest != 3 {
		t.Fatalf("apply = %v %d %v", applied, highest, err)
	}
	added, retired, err := s.PluginRoots()
	if err != nil || len(added) != 1 || added[0].KeyID != "gwroot-3" || !reflect.DeepEqual(retired, []string{"gwroot-1"}) {
		t.Fatalf("roots = %v %v %v", added, retired, err)
	}
	revs, _ := s.PluginRevocations()
	if len(revs["gwroot"]) != 2 {
		t.Fatalf("revocations = %v", revs)
	}

	// The same or a lower serial is a replay and changes nothing.
	replay := u
	replay.Floors = []extapi.Floor{{ID: "acme_lic", MinVersion: "9.0.0"}}
	for _, serial := range []uint64{3, 2} {
		replay.Serial = serial
		applied, highest, err = s.ApplyPluginTrust("trust_update", "gwroot", replay)
		if err != nil || applied || highest != 3 {
			t.Fatalf("replay %d = %v %d %v", serial, applied, highest, err)
		}
	}
	if floors, _ := s.PluginFloors(); floors["acme_lic"] != "2.0.0" {
		t.Fatalf("a replay moved the floor: %v", floors)
	}
	// Serials are per issuer.
	if applied, _, _ := s.ApplyPluginTrust("crl:gwroot", "gwroot", extapi.TrustUpdate{Serial: 1, Revoke: u.Revoke}); !applied {
		t.Fatal("another issuer's first list was refused")
	}
	if revs, _ := s.PluginRevocations(); len(revs["gwroot"]) != 2 {
		t.Fatalf("a repeated revocation was recorded twice: %v", revs)
	}
	serials, _ := s.PluginTrustSerials()
	if serials["trust_update"] != 3 || serials["crl:gwroot"] != 1 {
		t.Fatalf("serials = %v", serials)
	}

	// A root key id never names a second key; the whole update rolls back.
	conflict := extapi.TrustUpdate{Serial: 4, AddRoots: []extapi.PublicKey{rootKey("gwroot-3", 9)}, Floors: []extapi.Floor{{ID: "acme_lic", MinVersion: "3.0.0"}}}
	if _, _, err := s.ApplyPluginTrust("trust_update", "gwroot", conflict); !errors.Is(err, ErrPluginRootConflict) {
		t.Fatalf("conflicting root = %v", err)
	}
	if floors, _ := s.PluginFloors(); floors["acme_lic"] != "2.0.0" {
		t.Fatalf("a failed update moved the floor: %v", floors)
	}
	if serials, _ := s.PluginTrustSerials(); serials["trust_update"] != 3 {
		t.Fatalf("a failed update recorded its serial: %v", serials)
	}

	// Retiring an added root keeps the row; adding it again with the same key revives it.
	if _, _, err := s.ApplyPluginTrust("trust_update", "gwroot", extapi.TrustUpdate{Serial: 5, RetireRoots: []string{"gwroot-3"}}); err != nil {
		t.Fatal(err)
	}
	if added, retired, _ := s.PluginRoots(); len(added) != 0 || len(retired) != 2 {
		t.Fatalf("after retiring: %v %v", added, retired)
	}
	if _, _, err := s.ApplyPluginTrust("trust_update", "gwroot", extapi.TrustUpdate{Serial: 6, AddRoots: []extapi.PublicKey{rootKey("gwroot-3", 3)}}); err != nil {
		t.Fatal(err)
	}
	if added, _, _ := s.PluginRoots(); len(added) != 1 {
		t.Fatalf("re-added root missing: %v", added)
	}
}

func TestRaisePluginFloorOnlyRises(t *testing.T) {
	s := newTestStore(t)
	steps := []struct {
		version string
		raised  bool
		floor   string
	}{
		{"1.2.0", true, "1.2.0"},
		{"1.1.9", false, "1.2.0"},
		{"1.2.0", false, "1.2.0"},
		{"1.2.0-rc.1", false, "1.2.0"},
		{"1.10.0", true, "1.10.0"},
	}
	for _, step := range steps {
		raised, err := s.RaisePluginFloor("acme_lic", step.version)
		if err != nil || raised != step.raised {
			t.Fatalf("raise to %s = %v %v", step.version, raised, err)
		}
		if floors, _ := s.PluginFloors(); floors["acme_lic"] != step.floor {
			t.Fatalf("after %s floor = %v", step.version, floors)
		}
	}
	if _, err := s.RaisePluginFloor("acme_lic", "latest"); err == nil {
		t.Fatal("a floor that is not a version was accepted")
	}
	// A stored floor this build cannot parse is replaced by a valid one.
	if err := s.db.Model(&PluginFloor{}).Where("id = ?", "acme_lic").Update("min_version", "garbage").Error; err != nil {
		t.Fatal(err)
	}
	if raised, err := s.RaisePluginFloor("acme_lic", "0.1.0"); err != nil || !raised {
		t.Fatalf("unparsable floor kept: %v %v", raised, err)
	}
}

func TestPluginMarkers(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []extapi.Marker{
		{Name: "plugin.acme_x.data", Version: 2, WrittenBy: "v26.12.0"},
		{Name: "pstate.availability", Version: 1, WrittenBy: "v26.12.0"},
		{Name: "plugin.acme_x.data", Version: 3, WrittenBy: "v27.1.0"},
	} {
		if err := s.SetPluginMarker(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.PluginMarkers()
	want := []extapi.Marker{{Name: "plugin.acme_x.data", Version: 3, WrittenBy: "v27.1.0"}, {Name: "pstate.availability", Version: 1, WrittenBy: "v26.12.0"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("markers = %v %v", got, err)
	}
	if err := s.DeletePluginMarker("plugin.acme_x.data"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PluginMarkers(); len(got) != 1 {
		t.Fatalf("after delete: %v", got)
	}
}

func TestReadPluginMarkersNeverWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpuworks.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPluginMarker(extapi.Marker{Name: "pstate.node_holds", Version: 2, WrittenBy: "v27.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPluginMarkers(path)
	if err != nil || len(got) != 1 || got[0].Version != 2 {
		t.Fatalf("markers = %v %v", got, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("reading markers changed the database file")
	}

	if _, err := ReadPluginMarkers(filepath.Join(dir, "missing.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database = %v", err)
	}
	if _, err := ReadPluginMarkers(""); err == nil {
		t.Fatal("an in-memory database was accepted")
	}

	// A database written before the marker table existed has no markers.
	old, err := Open(filepath.Join(dir, "old.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := old.db.Migrator().DropTable(&PluginMarker{}); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if got, err := ReadPluginMarkers(filepath.Join(dir, "old.db")); err != nil || len(got) != 0 {
		t.Fatalf("pre-marker database = %v %v", got, err)
	}
}

// The markers pushed state writes (pstate_marker) are read together with
// the plugin host's, sorted, online and offline, so the downgrade check and
// `gpuworks plugin compat` see both.
func TestPluginMarkersIncludePushedStateMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpuworks.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPluginMarker(extapi.Marker{Name: "plugin.acme_x.data", Version: 1, WrittenBy: "v27.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WritePushedState(context.Background(), &pstate.Batch{Markers: []pstate.Marker{{Name: "pstate.banners", Version: 2, WrittenBy: "v27.4.0"}}}); err != nil {
		t.Fatal(err)
	}
	want := []extapi.Marker{{Name: "plugin.acme_x.data", Version: 1, WrittenBy: "v27.0.0"}, {Name: "pstate.banners", Version: 2, WrittenBy: "v27.4.0"}}
	if got, err := s.PluginMarkers(); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("online markers = %v %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPluginMarkers(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("offline markers = %v %v", got, err)
	}
	// A database from before pushed state has only the host's markers.
	old, err := Open(filepath.Join(dir, "old.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := old.db.Migrator().DropTable(&pstate.Marker{}); err != nil {
		t.Fatal(err)
	}
	if got, err := old.PluginMarkers(); err != nil || len(got) != 0 {
		t.Fatalf("pre-pushed-state markers = %v %v", got, err)
	}
	old.Close()
}
