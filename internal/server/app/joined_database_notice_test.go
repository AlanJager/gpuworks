//go:build linux && dqlite

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJoinedDatabaseListenNoticeSeparatesIdentityFilesFromRaft(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"info.yaml", "cluster.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "dqlite-lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notice := joinedDatabaseListenNotice(dir)
	for _, part := range []string{"only identity files", "database address will not listen", "material route will not be mounted"} {
		if !strings.Contains(notice, part) {
			t.Fatalf("identity-only directory notice = %q", notice)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata1"), []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	notice = joinedDatabaseListenNotice(dir)
	if !strings.Contains(notice, "no raft log or snapshot") || !strings.Contains(notice, "database address will not listen") {
		t.Fatalf("metadata-only directory notice = %q", notice)
	}
	if err := os.WriteFile(filepath.Join(dir, "0000000000000001-0000000000000001"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if notice := joinedDatabaseListenNotice(dir); notice != "" {
		t.Fatalf("raft directory was reported as closed: %q", notice)
	}
}
