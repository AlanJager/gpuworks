//go:build linux && dqlite

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterJoinRejectsInvalidArgumentsBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing identity", []string{"--address", "127.0.0.1:19001"}, "--cluster-id"},
		{"missing fresh invitation", []string{"--address", "127.0.0.1:19001", "--cluster-id", "abc", "--ca-fingerprint", "ff", "--enrollment-url", "https://127.0.0.1:9443"}, "--token-file"},
		{"positional", []string{"--address", "127.0.0.1:19001", "--cluster-id", "abc", "--ca-fingerprint", "ff", "--enrollment-url", "https://127.0.0.1:9443", "extra"}, "positional"},
		{"timeout", []string{"--address", "127.0.0.1:19001", "--cluster-id", "abc", "--ca-fingerprint", "ff", "--enrollment-url", "https://127.0.0.1:9443", "--timeout", "0s"}, "positive timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			args := append([]string{"--data", dir}, tc.args...)
			err := clusterJoinCommand(context.Background(), args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments modified filesystem: %v", err)
			}
		})
	}
}

func TestClusterJoinTokenFileMustBePrivateAndBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invitation")
	if err := os.WriteFile(path, []byte("private-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := readClusterJoinToken(path); err != nil || token != "private-token" {
		t.Fatalf("private token = %q, %v", token, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readClusterJoinToken(link); err == nil {
		t.Fatal("symlink token accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readClusterJoinToken(path); err == nil {
		t.Fatal("world-readable token accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readClusterJoinToken(path); err == nil {
		t.Fatal("oversized token accepted")
	}
	if err := os.WriteFile(path, []byte("two tokens"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readClusterJoinToken(path); err == nil {
		t.Fatal("multi-token file accepted")
	}
}
