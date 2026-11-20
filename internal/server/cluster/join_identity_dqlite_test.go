//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"
)

func TestPrepareJoinIdentityPreservesPublishedIdentity(t *testing.T) {
	requireDatabaseHelper(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "raft")
	address, seeds := "127.0.0.1:9101", []string{"127.0.0.1:9100"}
	id, err := PrepareJoinIdentity(t.Context(), dir, address, seeds)
	if err != nil || id == 0 {
		t.Fatalf("prepare: %d %v", id, err)
	}
	before := map[string][]byte{}
	for _, name := range []string{"info.yaml", "cluster.yaml", "join"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
		stat, err := os.Stat(filepath.Join(dir, name))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatalf("private %s: %v", name, err)
		}
	}
	var stored client.NodeInfo
	if err := yaml.Unmarshal(before["info.yaml"], &stored); err != nil || stored.ID != id || stored.Address != address {
		t.Fatalf("stored identity: %+v %v", stored, err)
	}
	// Discovery seed changes do not overwrite the existing upstream cache or ID.
	again, err := PrepareJoinIdentity(t.Context(), dir, address, []string{"127.0.0.1:9102"})
	if err != nil || again != id {
		t.Fatalf("retry: %d %v", again, err)
	}
	for name, expected := range before {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("retry rewrote %s: %v", name, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "join")); err != nil {
		t.Fatal(err)
	}
	if again, err := PrepareJoinIdentity(t.Context(), dir, address, seeds); err != nil || again != id {
		t.Fatalf("established identity: %d %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "join")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry reintroduced join marker")
	}
	marker := filepath.Join(dir, "join")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCommittedJoinMarker(t.Context(), dir, []Member{{ID: id, Address: "127.0.0.1:9199"}}); err == nil {
		t.Fatal("wrong address reconciled")
	}
	if err := os.WriteFile(marker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCommittedJoinMarker(t.Context(), dir, []Member{{ID: id, Address: address}}); err == nil {
		t.Fatal("marker content reconciled")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "x" {
		t.Fatalf("invalid marker changed: %q %v", got, err)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCommittedJoinMarker(t.Context(), dir, []Member{{ID: id, Address: address}}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCommittedJoinMarker(t.Context(), dir, nil); err != nil {
		t.Fatal("missing marker is already reconciled", err)
	}
	if _, err := PrepareJoinIdentity(t.Context(), dir, "127.0.0.1:9103", seeds); err == nil {
		t.Fatal("changed member address accepted")
	}
}

func TestPrepareJoinIdentityRejectsDamagedExistingState(t *testing.T) {
	requireDatabaseHelper(t)
	for _, name := range []string{"info missing", "cache missing", "both missing", "empty directory", "bootstrap", "wrong address", "symlink", "cache duplicate", "cache identity conflict", "cache unknown field", "marker content"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "raft")
			address, seeds := "127.0.0.1:9101", []string{"127.0.0.1:9100"}
			id, err := PrepareJoinIdentity(t.Context(), dir, address, seeds)
			if err != nil {
				t.Fatal(err)
			}
			write := func(file string, data []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			remove := func(file string) {
				t.Helper()
				if err := os.Remove(filepath.Join(dir, file)); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "info missing":
				remove("info.yaml")
			case "cache missing":
				remove("cluster.yaml")
			case "both missing":
				remove("info.yaml")
				remove("cluster.yaml")
				write("0000000000000001-0000000000000002", []byte("preserved raft data"))
			case "empty directory":
				remove("info.yaml")
				remove("cluster.yaml")
				remove("join")
			case "bootstrap":
				write("info.yaml", []byte("ID: 1\nAddress: 127.0.0.1:9101\n"))
			case "wrong address":
				write("info.yaml", []byte("ID: 9\nAddress: 127.0.0.1:9199\n"))
			case "symlink":
				remove("info.yaml")
				if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(dir, "info.yaml")); err != nil {
					t.Fatal(err)
				}
			case "cache duplicate":
				write("cluster.yaml", []byte("- ID: 2\n  Address: 127.0.0.1:9100\n- ID: 2\n  Address: 127.0.0.1:9102\n"))
			case "cache identity conflict":
				data, _ := yaml.Marshal([]client.NodeInfo{{ID: id, Address: "127.0.0.1:9100"}})
				write("cluster.yaml", data)
			case "cache unknown field":
				write("cluster.yaml", []byte("- ID: 2\n  Address: 127.0.0.1:9100\n  Unexpected: true\n"))
			case "marker content":
				write("join", []byte("invalid"))
			}
			if got, err := PrepareJoinIdentity(t.Context(), dir, address, seeds); err == nil || got != 0 {
				t.Fatalf("damaged state accepted: %d %v", got, err)
			}
			if name == "both missing" {
				if _, err := os.Lstat(filepath.Join(dir, "info.yaml")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing identity regenerated")
				}
			}
		})
	}
}

func TestPrepareJoinIdentityRejectsBeforePublication(t *testing.T) {
	requireDatabaseHelper(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "raft")
	for _, seeds := range [][]string{nil, {"127.0.0.1:9101"}, {"HOST:9100"}, {"127.0.0.1:9100", "127.0.0.1:9100"}} {
		if id, err := PrepareJoinIdentity(t.Context(), dir, "127.0.0.1:9101", seeds); err == nil || id != 0 {
			t.Fatal("invalid seeds accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PrepareJoinIdentity(ctx, dir, "127.0.0.1:9101", []string{"127.0.0.1:9100"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid preparation wrote files: %v %v", entries, err)
	}
}
