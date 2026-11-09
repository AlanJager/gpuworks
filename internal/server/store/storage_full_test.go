package store

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// TestIsStorageFullOnARealFullDatabase fills a real database file: the page
// cap stands in for the disk, and SQLite answers a write past it with the
// same SQLITE_FULL it raises when the filesystem has no room.
func TestIsStorageFullOnARealFullDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	var pages int64
	if err := s.db.Raw("PRAGMA page_count").Scan(&pages).Error; err != nil {
		t.Fatalf("page_count: %v", err)
	}
	// The writer pool holds a single connection, so the cap stays on it.
	if err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)).Error; err != nil {
		t.Fatalf("max_page_count: %v", err)
	}
	var writeErr error
	for i := 0; i < 10000 && writeErr == nil; i++ {
		_, writeErr = s.RegisterNode(fmt.Sprintf("uuid-%d", i), HashToken(fmt.Sprintf("token-%d", i)), strings.Repeat("h", 200))
	}
	if writeErr == nil {
		t.Fatal("writes past max_page_count kept succeeding")
	}
	if !IsStorageFull(writeErr) {
		t.Fatalf("IsStorageFull(%v) = false", writeErr)
	}
	if !strings.Contains(writeErr.Error(), "database or disk is full") {
		t.Fatalf("error = %q, want SQLite's full-disk text", writeErr)
	}
}

func TestIsStorageFullClassifiesOnlyFullDisks(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	// A real SQLite error that is not SQLITE_FULL.
	constraint := s.db.Exec("CREATE TABLE t (x INTEGER PRIMARY KEY); INSERT INTO t VALUES (1); INSERT INTO t VALUES (1)").Error
	if constraint == nil {
		t.Fatal("duplicate primary key was accepted")
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("database or disk is full"), false},
		{"not found", ErrNotFound, false},
		{"enospc", syscall.ENOSPC, true},
		{"wrapped enospc", fmt.Errorf("write log: %w", &fs.PathError{Op: "write", Path: "/data/x", Err: syscall.ENOSPC}), true},
		{"other errno", &fs.PathError{Op: "open", Path: "/data/x", Err: syscall.EACCES}, false},
		{"sqlite constraint", constraint, false},
	}
	for _, tc := range cases {
		if got := IsStorageFull(tc.err); got != tc.want {
			t.Errorf("%s: IsStorageFull(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
