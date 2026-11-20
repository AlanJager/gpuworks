//go:build linux && dqlite

package cli

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDatabaseAddressMustBeBoundOnThisHost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	occupied := listener.Addr().String()
	defer listener.Close()
	if err := requireLocalDatabaseAddress(occupied); err == nil || !strings.Contains(err.Error(), "cannot be bound") {
		t.Fatalf("occupied port: %v", err)
	}
	if err := requireLocalDatabaseAddress("203.0.113.10:19001"); err == nil || !strings.Contains(err.Error(), "not a local address") {
		t.Fatalf("foreign address: %v", err)
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	available := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}
	if err := requireLocalDatabaseAddress(available); err != nil {
		t.Fatal(err)
	}
	again, err := net.Listen("tcp", available)
	if err != nil {
		t.Fatalf("address check kept the port: %v", err)
	}
	again.Close()
}

func TestClusterJoinRejectsAForeignDatabaseAddressBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "invitation")
	if err := os.WriteFile(token, []byte("invite-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	err := clusterJoinCommand(context.Background(), []string{
		"--data", data,
		"--address", "203.0.113.10:19001",
		"--cluster-id", "abc",
		"--ca-fingerprint", "ff",
		"--enrollment-url", "https://203.0.113.10:19443/api/v1/cluster/enrollment/certificate",
		"--token-file", token,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a local address") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Lstat(data); !os.IsNotExist(statErr) {
		t.Fatalf("foreign address wrote the data directory: %v", statErr)
	}
}

func TestClusterAddressRejectsAForeignDatabaseAddressBeforeWriting(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	err := clusterAddressCommand(context.Background(), []string{
		"--data", data,
		"--offline",
		"--address", "203.0.113.10:19001",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a local address") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Lstat(data); !os.IsNotExist(statErr) {
		t.Fatalf("foreign address wrote the data directory: %v", statErr)
	}
}
