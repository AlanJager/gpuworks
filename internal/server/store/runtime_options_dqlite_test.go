//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func dqliteTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// The single-member replicated open applies the same core and extension
// schemas as the SQLite open, keeps them across a restart, and refuses a
// member whose files say it is not a single member.
func TestNativeOpenRuntimeWithAppliesExtensionSchemas(t *testing.T) {
	ctx := dqliteTestContext(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gpuworks.db")
	opts := OpenOptions{Extensions: []MigrationSet{quotaSet()}}
	st, err := OpenRuntimeWith(ctx, path, bus.New(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := markers(t, st, "quota"); len(got) != 2 {
		t.Fatalf("recorded steps = %+v, want 2", got)
	}
	if _, err := st.ExtensionTables("quota"); err != nil {
		t.Fatalf("extension tables: %v", err)
	}
	if err := st.CheckBusinessReads(ctx); err != nil {
		t.Fatalf("business reads on a fresh single member: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the replicated open created a SQLite file: %v", err)
	}
	info, err := os.Stat(path + ".dqlite")
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("native directory = %v %v, want owner-only", info, err)
	}
	if _, err := OpenRuntimeWith(ctx, path, bus.New(), opts); err == nil || !strings.Contains(err.Error(), "lock runtime database") {
		t.Fatalf("second owner = %v, want a runtime lock refusal", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// A restart applies nothing again and adds a later step.
	longer := quotaSet()
	longer.Migrations = append(longer.Migrations, createStep(3, "items note", "ALTER TABLE ee_quota_items ADD COLUMN note TEXT"))
	again, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{Extensions: []MigrationSet{longer}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := markers(t, again, "quota"); len(got) != 3 {
		t.Fatalf("steps after reopen = %+v, want 3", got)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}

	// An older binary must not run on the newer database.
	if _, err := OpenRuntimeWith(ctx, path, bus.New(), opts); !errors.Is(err, ErrNamespaceTooNew) {
		t.Fatalf("older extension set = %v, want ErrNamespaceTooNew", err)
	}
	// The refused open released its lease and its database.
	final, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{Extensions: []MigrationSet{longer}})
	if err != nil {
		t.Fatalf("open after a refused open: %v", err)
	}
	if err := final.Close(); err != nil {
		t.Fatal(err)
	}

	// Group or world access to the native directory is refused before dqlite starts.
	if err := os.Chmod(path+".dqlite", 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{}); err == nil || !strings.Contains(err.Error(), "only its owner can access") {
		t.Fatalf("group-readable native directory = %v", err)
	}
	if err := os.Chmod(path+".dqlite", 0o700); err != nil {
		t.Fatal(err)
	}

	// A join marker never reaches the single-member open.
	if err := os.WriteFile(path+".join", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{}); err == nil || !strings.Contains(err.Error(), "cluster join is incomplete") {
		t.Fatalf("open beside a join marker = %v", err)
	}
}

// A failing extension step fails the open and leaves neither its table nor
// its marker in the replicated database.
func TestNativeOpenRuntimeWithRollsBackAFailedExtensionStep(t *testing.T) {
	ctx := dqliteTestContext(t)
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	broken := quotaSet()
	broken.Migrations[1] = createStep(2, "items key index", "CREATE INDEX ee_quota_items_by_key ON ee_quota_missing (key)")
	if _, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{Extensions: []MigrationSet{broken}}); err == nil {
		t.Fatal("open with a failing extension step succeeded")
	}
	st, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{Extensions: []MigrationSet{quotaSet()}})
	if err != nil {
		t.Fatalf("open after the failed step: %v", err)
	}
	defer st.Close()
	got := markers(t, st, "quota")
	if len(got) != 2 || got[0].Name != "items" || got[1].Name != "items key index" {
		t.Fatalf("markers = %+v", got)
	}
}
