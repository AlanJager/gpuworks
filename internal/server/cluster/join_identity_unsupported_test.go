//go:build !linux || !dqlite

package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareJoinIdentityUnsupportedDoesNotWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "raft")
	if id, err := PrepareJoinIdentity(t.Context(), dir, "127.0.0.1:9101", []string{"127.0.0.1:9100"}); id != 0 || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unsupported: %d %v", id, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported preparation wrote state")
	}
	kept := t.TempDir()
	marker := filepath.Join(kept, "join")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCommittedJoinMarker(t.Context(), kept, []Member{{ID: 9, Address: "127.0.0.1:9101"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatal("unsupported reconciliation removed the join marker", err)
	}
}
