package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestAdoptExtensionSchemasRequiresEveryRecordedStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	// A database no extension opened: nothing recorded, nothing adopted.
	plain := mustOpenWithSets(t, path)
	if err := plain.AdoptExtensionSchemas(context.Background(), nil); err != nil {
		t.Fatalf("adopt no sets: %v", err)
	}
	err := plain.AdoptExtensionSchemas(context.Background(), []MigrationSet{quotaSet()})
	if !errors.Is(err, ErrExtensionSchemaNotApplied) {
		t.Fatalf("adopt on a database without the namespace table = %v, want ErrExtensionSchemaNotApplied", err)
	}
	if _, tableErr := plain.ExtensionTables("quota"); tableErr == nil {
		t.Fatal("a refused adoption must not hand out extension tables")
	}

	// Only the first step applied: the second is missing.
	partial := quotaSet()
	partial.Migrations = partial.Migrations[:1]
	st, err := reopen(t, plain, path, partial)
	if err != nil {
		t.Fatal(err)
	}
	bound := &Store{db: st.db}
	err = bound.AdoptExtensionSchemas(context.Background(), []MigrationSet{quotaSet()})
	if !errors.Is(err, ErrExtensionSchemaNotApplied) || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("adopt with step 2 missing = %v", err)
	}
	if len(bound.extensions) != 0 {
		t.Fatal("refused adoption recorded extension sets")
	}
	// A newer database than this binary.
	err = bound.AdoptExtensionSchemas(context.Background(), []MigrationSet{{Namespace: "quota", TablePrefix: "ee_quota_"}})
	if !errors.Is(err, ErrNamespaceTooNew) {
		t.Fatalf("adopt an older set over a newer database = %v, want ErrNamespaceTooNew", err)
	}
	// A step recorded under another name.
	renamed := partial
	renamed.Migrations = []Migration{createStep(1, "other name", "SELECT 1")}
	if err := bound.AdoptExtensionSchemas(context.Background(), []MigrationSet{renamed}); err == nil || !strings.Contains(err.Error(), `recorded as "items"`) {
		t.Fatalf("adopt a renamed step = %v", err)
	}

	// Fully applied: adopted without writing.
	full, err := reopen(t, st, path, quotaSet())
	if err != nil {
		t.Fatal(err)
	}
	before := markers(t, full, "quota")
	bound = &Store{db: full.db}
	if err := bound.AdoptExtensionSchemas(context.Background(), []MigrationSet{quotaSet()}); err != nil {
		t.Fatalf("adopt applied set: %v", err)
	}
	if _, err := bound.ExtensionTables("quota"); err != nil {
		t.Fatalf("extension tables after adoption: %v", err)
	}
	if after := markers(t, full, "quota"); len(after) != len(before) || !after[1].AppliedAt.Equal(before[1].AppliedAt) {
		t.Fatalf("adoption changed markers: %+v -> %+v", before, after)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Store{db: full.db}).AdoptExtensionSchemas(canceled, []MigrationSet{quotaSet()}); err == nil {
		t.Fatal("adoption ignored a canceled context")
	}
}

func TestOpenRuntimeWithRejectsInvalidSetsBeforeTouchingDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpuworks.db")
	bad := []MigrationSet{{Namespace: "Bad", TablePrefix: "ee_bad_"}}
	if _, err := OpenRuntimeWith(context.Background(), path, bus.New(), OpenOptions{Extensions: bad}); err == nil {
		t.Fatal("invalid extension set accepted")
	}
	if _, err := OpenExpandedRuntimeWith(context.Background(), path, bus.New(), OpenOptions{Extensions: bad}); err == nil {
		t.Fatal("invalid extension set accepted by the expanded open")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused open left files: %v", entries)
	}
}

func TestCheckRuntimeDirectoryPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gpuworks.db")
	if err := checkRuntimeDirectoryPolicy(path); err != nil {
		t.Fatalf("owner-only directory without native files: %v", err)
	}
	if err := checkRuntimeDirectoryPolicy(""); err != nil {
		t.Fatalf("in-memory database: %v", err)
	}
	native := path + ".dqlite"
	if err := os.Mkdir(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntimeDirectoryPolicy(path); err != nil {
		t.Fatalf("owner-only native directory: %v", err)
	}
	if err := os.Chmod(native, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntimeDirectoryPolicy(path); err == nil {
		t.Fatal("group-readable native directory accepted")
	}
	if err := os.Chmod(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntimeDirectoryPolicy(path); err != nil {
		t.Fatalf("owner-only policy leaves the directory mode to the installation: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(native); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntimeDirectoryPolicy(path); err == nil {
		t.Fatal("a regular file in place of the native directory accepted")
	}
	if err := checkRuntimeDirectoryPolicy(filepath.Join(dir, "missing", "gpuworks.db")); err == nil {
		t.Fatal("missing data directory accepted")
	}
}
