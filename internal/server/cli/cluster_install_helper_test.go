package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDatabaseHelper(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, content, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClusterInstallCopiesDatabaseHelperBesideServerWithItsDigest(t *testing.T) {
	o := offlineInstallOptions(t)
	helper := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{7}, 40)...)
	o.DatabaseHelper = writeDatabaseHelper(t, helper)
	server, err := os.ReadFile(o.Binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
	type transfer struct {
		host, command string
		payload       []byte
	}
	var transfers []transfer
	stop := errors.New("stop after transfers")
	err = installCluster(t.Context(), o, io.Discard, func(context.Context, string, io.Reader) ([]byte, error) {
		return nil, stop
	}, func(ctx context.Context, host, command string, input io.Reader) error {
		if input == nil {
			return nil
		}
		raw, err := io.ReadAll(input)
		if err != nil {
			t.Fatal(err)
		}
		transfers = append(transfers, transfer{host, command, raw})
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("install stopped with %v", err)
	}
	if len(transfers) != 6 {
		t.Fatalf("transfers = %d, want the server and the helper on three hosts", len(transfers))
	}
	for i, got := range transfers {
		host := o.Hosts[i/2]
		name, payload := "gpuworks", server
		if i%2 == 1 {
			name, payload = "gpuworks-dqlite", helper
		}
		target := o.Dir + "/" + name
		want := "set -C; cat > '" + target + "'; chmod 700 '" + target + "'; echo '" + digest(payload) + "  " + target + "' | sha256sum -c - >/dev/null"
		if got.host != host || !bytes.Equal(got.payload, payload) || !strings.HasSuffix(got.command, want) || !strings.HasPrefix(got.command, "set -eu; umask 077; ") {
			t.Fatalf("transfer %d = host %s command %q", i, got.host, got.command)
		}
		// Only the server transfer creates the installation directory.
		if created := strings.Contains(got.command, "mkdir"); created != (i%2 == 0) {
			t.Fatalf("transfer %d directory creation = %v: %q", i, created, got.command)
		}
	}
}

func TestClusterInstallWithoutDatabaseHelperCopiesOnlyTheServer(t *testing.T) {
	o := offlineInstallOptions(t)
	copies := 0
	stop := errors.New("stop after transfers")
	err := installCluster(t.Context(), o, io.Discard, func(context.Context, string, io.Reader) ([]byte, error) {
		return nil, stop
	}, func(ctx context.Context, host, command string, input io.Reader) error {
		if input != nil {
			copies++
			if strings.Contains(command, "gpuworks-dqlite") {
				t.Fatalf("helper copied without --database-helper: %q", command)
			}
		}
		return nil
	})
	if !errors.Is(err, stop) || copies != 3 {
		t.Fatalf("copies=%d err=%v", copies, err)
	}
}

func TestClusterInstallRejectsUnusableDatabaseHelperBeforeRemoteWork(t *testing.T) {
	for name, path := range map[string]string{
		"not an executable image": writeDatabaseHelper(t, []byte("#!/bin/sh\nexit 0\n# padding padding")),
		"truncated":               writeDatabaseHelper(t, []byte{0x7f, 'E', 'L', 'F'}),
		"missing":                 filepath.Join(t.TempDir(), "absent"),
	} {
		t.Run(name, func(t *testing.T) {
			o := offlineInstallOptions(t)
			o.DatabaseHelper = path
			err := installCluster(t.Context(), o, io.Discard, func(context.Context, string, io.Reader) ([]byte, error) {
				t.Fatal("node command ran with an unusable helper")
				return nil, nil
			}, func(context.Context, string, string, io.Reader) error {
				t.Fatal("remote work started with an unusable helper")
				return nil
			})
			if err == nil {
				t.Fatal("unusable helper accepted")
			}
			if name != "missing" && err.Error() != "--database-helper must be a Linux ELF executable" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestClusterInstallFailedHelperTransferStopsBeforeNodeCommands(t *testing.T) {
	o := offlineInstallOptions(t)
	o.DatabaseHelper = writeDatabaseHelper(t, append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 28)...))
	denied := errors.New("digest mismatch")
	copies := 0
	err := installCluster(t.Context(), o, io.Discard, func(context.Context, string, io.Reader) ([]byte, error) {
		t.Fatal("node command ran after a failed helper transfer")
		return nil, nil
	}, func(ctx context.Context, host, command string, input io.Reader) error {
		if input == nil {
			return nil
		}
		copies++
		if strings.Contains(command, "gpuworks-dqlite") {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) || copies != 2 {
		t.Fatalf("copies=%d err=%v", copies, err)
	}
}

func TestClusterInstallDatabaseHelperFlag(t *testing.T) {
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("private-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.1,192.0.2.2,192.0.2.3", "--ssh-user", "operator", "--dir", "/home/operator/cluster"}
	t.Chdir(t.TempDir())
	options, err := parseClusterInstall(append(append([]string{}, base...), "--password-file", password, "--database-helper", "bin/gpuworks-dqlite"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(options.DatabaseHelper) || filepath.Base(options.DatabaseHelper) != "gpuworks-dqlite" {
		t.Fatalf("helper path = %q", options.DatabaseHelper)
	}
	options, err = parseClusterInstall(append(append([]string{}, base...), "--password-file", password), io.Discard)
	if err != nil || options.DatabaseHelper != "" {
		t.Fatalf("default helper = %q err=%v", options.DatabaseHelper, err)
	}
	_, err = parseClusterOptions(append(append([]string{}, base...), "--database-helper", "bin/gpuworks-dqlite"), io.Discard, true)
	if err == nil || err.Error() != "gpuworks cluster check does not use --database-helper" {
		t.Fatalf("check accepted a helper: %v", err)
	}
}

func TestClusterInstallPrepareAcceptsOnlyCopiedExecutables(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		dirs  []string
		links []string
		want  bool
	}{
		{"server only", []string{"gpuworks"}, nil, nil, true},
		{"server and helper", []string{"gpuworks", "gpuworks-dqlite"}, nil, nil, true},
		{"helper only", []string{"gpuworks-dqlite"}, nil, nil, false},
		{"empty", nil, nil, nil, false},
		{"foreign file", []string{"gpuworks", "gpuworks-dqlite", "notes"}, nil, nil, false},
		{"prior data", []string{"gpuworks"}, []string{"data"}, nil, false},
		{"helper is a directory", []string{"gpuworks"}, []string{"gpuworks-dqlite"}, nil, false},
		{"helper is a link", []string{"gpuworks"}, nil, []string{"gpuworks-dqlite"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range tc.dirs {
				if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range tc.links {
				if err := os.Symlink("gpuworks", filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := clusterInstallFreshEntries(entries); got != tc.want {
				t.Fatalf("fresh = %v, want %v", got, tc.want)
			}
		})
	}
}
