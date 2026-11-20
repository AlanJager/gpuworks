//go:build linux && dqlite

package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A data directory that belongs to a cluster member must never be started as
// a standalone server, and a member must never mint a credential key: a new
// key seals credentials no other member can open. Each case fails New before
// it writes anything.
func TestNewRefusesAClusterMemberWithoutItsMaterials(t *testing.T) {
	multiMember := "- ID: 1\n  Address: 127.0.0.1:9001\n  Role: 0\n- ID: 2\n  Address: 127.0.0.1:9011\n  Role: 0\n"
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dir string) Config
		want    string
	}{
		{"join marker without enrollment materials", func(t *testing.T, dir string) Config {
			mustMkdir(t, filepath.Join(dir, "gpuworks.db.join"))
			return Config{DataDir: dir}
		}, "joined runtime requires local enrollment materials"},
		{"established member without enrollment materials", func(t *testing.T, dir string) Config {
			mustMkdir(t, filepath.Join(dir, "gpuworks.db.dqlite"))
			mustWrite(t, filepath.Join(dir, "gpuworks.db.dqlite", "cluster.yaml"), multiMember)
			return Config{DataDir: dir}
		}, "expanded seed requires local enrollment materials"},
		{"join marker with the database outside the data directory", func(t *testing.T, dir string) Config {
			other := t.TempDir()
			mustMkdir(t, filepath.Join(other, "elsewhere.db.join"))
			return Config{DataDir: dir, DBPath: filepath.Join(other, "elsewhere.db")}
		}, "a cluster member requires the database path inside its data directory"},
		{"established member with the database outside the data directory", func(t *testing.T, dir string) Config {
			other := t.TempDir()
			mustMkdir(t, filepath.Join(other, "elsewhere.db.dqlite"))
			mustWrite(t, filepath.Join(other, "elsewhere.db.dqlite", "cluster.yaml"), multiMember)
			return Config{DataDir: dir, DBPath: filepath.Join(other, "elsewhere.db")}
		}, "a cluster member requires the database path inside its data directory"},
		{"damaged membership record", func(t *testing.T, dir string) Config {
			mustMkdir(t, filepath.Join(dir, "gpuworks.db.dqlite"))
			mustWrite(t, filepath.Join(dir, "gpuworks.db.dqlite", "cluster.yaml"), "{not yaml")
			return Config{DataDir: dir}
		}, "classify runtime database"},
		{"single member whose enrollment materials are incomplete", func(t *testing.T, dir string) Config {
			mustMkdir(t, filepath.Join(dir, enrollmentDirectory))
			return Config{DataDir: dir}
		}, ""},
		{"restore still in progress", func(t *testing.T, dir string) Config {
			mustWrite(t, filepath.Join(dir, ".restore-in-progress"), "")
			mustMkdir(t, filepath.Join(dir, "gpuworks.db.join"))
			return Config{DataDir: dir}
		}, "data restoration is incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := tc.prepare(t, dir)
			before := listTree(t, dir)
			a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil {
				_ = a.Close()
				t.Fatal("New started a cluster member's data directory")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Lstat(filepath.Join(dir, "master.key")); !os.IsNotExist(statErr) {
				t.Fatalf("refused start left a credential key: %v", statErr)
			}
			if after := listTree(t, dir); after != before {
				t.Fatalf("refused start changed the data directory:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// listTree names every entry under dir, so a test can tell whether a call
// created, removed or resized anything.
func listTree(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		size := "-"
		if info.Mode().IsRegular() {
			size = strconv.FormatInt(info.Size(), 10)
		}
		out.WriteString(rel + " " + info.Mode().String() + " " + size + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}
