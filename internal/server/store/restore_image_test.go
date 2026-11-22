package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// restoreImageFixture returns a closed database image of the current schema
// that holds one node and two extension steps, as a backup's image does.
func restoreImageFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source, err := Open(filepath.Join(dir, "source.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.CreateNodePending("in-the-image"); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "image.db")
	if err := source.SnapshotTo(image); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	return image
}

// emptyRestoreTarget is a store on a database without any object, which is
// what a new private database member is before an image is loaded.
func emptyRestoreTarget(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "target.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return &Store{db: db}
}

func alterImage(t *testing.T, image string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", image)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// Leave the file as a backup holds it: everything in the main file.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
}

func targetObjects(t *testing.T, st *Store) int {
	t.Helper()
	var count int
	if err := st.db.Raw("SELECT count(*) FROM sqlite_schema").Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestRestoreDatabaseImageLoadsTheImageOnItsOwnSchema(t *testing.T) {
	image := restoreImageFixture(t)
	versions, err := ImageMigrationVersions(t.Context(), image)
	if err != nil || len(versions) != len(migrations) || versions[len(versions)-1] != LatestMigration() {
		t.Fatalf("image markers = %d (last %v), %v", len(versions), versions[len(versions)-1:], err)
	}
	if extensions, err := ImageExtensionMigrationVersions(t.Context(), image); err != nil || extensions != nil {
		t.Fatalf("an image no extension opened reports %v, %v", extensions, err)
	}
	target := emptyRestoreTarget(t)
	if err := target.RestoreDatabaseImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := target.db.Raw("SELECT name FROM nodes ORDER BY id").Scan(&names).Error; err != nil || !reflect.DeepEqual(names, []string{"in-the-image"}) {
		t.Fatalf("restored nodes = %v, %v", names, err)
	}
	if err := checkCurrentSchema(target.db); err != nil {
		t.Fatalf("a current image restores to a schema this executable opens: %v", err)
	}
	var triggers int
	if err := target.db.Raw("SELECT count(*) FROM sqlite_schema WHERE type = 'trigger'").Scan(&triggers).Error; err != nil || triggers != 6 {
		t.Fatalf("restored triggers = %d, %v; the executable's six are replayed", triggers, err)
	}
	// The target is loaded once; a second image is never laid over it.
	if err := target.RestoreDatabaseImage(t.Context(), image); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("an image was loaded over existing objects: %v", err)
	}
}

func TestRestoreDatabaseImageKeepsAnEarlierReleasesSchema(t *testing.T) {
	// The image of the release before this one: the upgrade-rollback case.
	image := restoreImageFixture(t)
	alterImage(t, image, "DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)")
	target := emptyRestoreTarget(t)
	if err := target.RestoreDatabaseImage(t.Context(), image); err != nil {
		t.Fatalf("an image one migration behind was refused: %v", err)
	}
	var markers int
	if err := target.db.Raw("SELECT count(*) FROM schema_migrations").Scan(&markers).Error; err != nil || markers != len(migrations)-1 {
		t.Fatalf("restored markers = %d, %v; no migration runs during a restore", markers, err)
	}
	if err := checkCurrentSchema(target.db); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("the restored database is the earlier release's, not this one's: %v", err)
	}
}

func TestRestoreDatabaseImageRefusals(t *testing.T) {
	cases := map[string]struct {
		alter []string
		want  error
		text  string
	}{
		"a marker this executable does not know": {alter: []string{"INSERT INTO schema_migrations(version, name) VALUES (99999, 'from_the_future')"}, want: ErrRestoreImageSchema},
		"a gap in the markers":                   {alter: []string{"DELETE FROM schema_migrations WHERE version = (SELECT min(version) FROM schema_migrations)"}, want: ErrRestoreImageSchema},
		"no markers":                             {alter: []string{"DELETE FROM schema_migrations"}, want: ErrRestoreImageSchema},
		"a view":                                 {alter: []string{"CREATE VIEW sneaky AS SELECT 1"}, text: `unsupported SQLite object "sneaky"`},
		"a trigger with its own body":            {alter: []string{"CREATE TRIGGER sneaky AFTER INSERT ON nodes BEGIN DELETE FROM users; END"}, text: `unsupported SQLite object "sneaky"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			image := restoreImageFixture(t)
			alterImage(t, image, c.alter...)
			target := emptyRestoreTarget(t)
			err := target.RestoreDatabaseImage(t.Context(), image)
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) || !strings.Contains(err.Error(), c.text) {
				t.Fatalf("restore = %v", err)
			}
			if objects := targetObjects(t, target); objects != 0 {
				t.Fatalf("a refused image left %d objects in the target", objects)
			}
		})
	}
	t.Run("a damaged file", func(t *testing.T) {
		image := restoreImageFixture(t)
		raw, err := os.ReadFile(image)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(image, raw[:len(raw)/2], 0600); err != nil {
			t.Fatal(err)
		}
		target := emptyRestoreTarget(t)
		if err := target.RestoreDatabaseImage(t.Context(), image); err == nil {
			t.Fatal("half an image was restored")
		}
		if objects := targetObjects(t, target); objects != 0 {
			t.Fatalf("a damaged image left %d objects in the target", objects)
		}
	})
	t.Run("not a regular file", func(t *testing.T) {
		if err := emptyRestoreTarget(t).RestoreDatabaseImage(t.Context(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("a directory was accepted as an image: %v", err)
		}
	})
}

func TestImageExtensionMigrationVersions(t *testing.T) {
	image := restoreImageFixture(t)
	alterImage(t, image,
		"CREATE TABLE namespace_migrations (namespace TEXT, version INTEGER, name TEXT, applied_at DATETIME, PRIMARY KEY (namespace, version))",
		"INSERT INTO namespace_migrations(namespace, version, name) VALUES ('license', 1, 'a'), ('license', 2, 'b'), ('quota', 1, 'c')")
	got, err := ImageExtensionMigrationVersions(t.Context(), image)
	if err != nil || !reflect.DeepEqual(got, map[string][]int{"license": {1, 2}, "quota": {1}}) {
		t.Fatalf("extension markers = %v, %v", got, err)
	}
}

func TestRevokeOtherClusterAdmissions(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	keep := admissions[1].KeyFingerprint
	revision := func() int64 {
		t.Helper()
		var value int64
		if err := st.db.Raw("SELECT revision FROM cluster_authorizations WHERE id = 1").Scan(&value).Error; err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := revision()
	if _, _, err := st.RevokeOtherClusterAdmissions(t.Context(), id.ClusterID, strings.Repeat("9", 64)); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("a key without an admission kept nothing: %v", err)
	}
	if got := revision(); got != before {
		t.Fatalf("a refused revocation moved the authorization revision %d -> %d", before, got)
	}
	if admission, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, admissions[0].KeyFingerprint); err != nil || admission.RevokedAt != nil {
		t.Fatalf("a refused revocation revoked the seed: %+v %v", admission, err)
	}
	addresses, revoked, err := st.RevokeOtherClusterAdmissions(t.Context(), id.ClusterID, keep)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"peer-1.example:9010", "seed.example:9010"}; !reflect.DeepEqual(addresses, want) {
		t.Fatalf("revoked addresses = %v, want %v", addresses, want)
	}
	if want := []string{admissions[0].KeyFingerprint, admissions[2].KeyFingerprint}; !reflect.DeepEqual(revoked, want) {
		t.Fatalf("revoked keys = %v, want %v in key order", revoked, want)
	}
	if got := revision(); got != before+1 {
		t.Fatalf("authorization revision %d -> %d, want one step", before, got)
	}
	if _, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, keep); err != nil {
		t.Fatalf("the restoring member lost its admission: %v", err)
	}
	// The same call again reports the same policy input and changes nothing.
	addresses, revoked, err = st.RevokeOtherClusterAdmissions(t.Context(), id.ClusterID, keep)
	if err != nil || len(addresses) != 0 || len(revoked) != 2 || revision() != before+1 {
		t.Fatalf("repeat = %v %v %v revision %d", addresses, revoked, err, revision())
	}
	// A revoked address can be claimed again by a member that joins anew.
	_, token, err := st.IssueClusterInvitation(t.Context(), id.ClusterID, 1, 60e9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), id.ClusterID, token, strings.Repeat("7", 64), "seed.example:9010"); err != nil {
		t.Fatalf("a revoked member's address cannot be joined again: %v", err)
	}
}
