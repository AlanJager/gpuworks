//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
)

// Both independent databases use the real bootstrap identity. Their native
// sockets must therefore be isolated by storage, not by changing Raft IDs.
func TestDatabaseIndependentTLSBootstrapStorage(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	firstListen, firstDial := databaseTestTLS(t)
	secondListen, secondDial := databaseTestTLS(t)
	firstConfig := databaseTLSConfig(t, firstListen, firstDial)
	secondConfig := databaseTLSConfig(t, secondListen, secondDial)
	open := func(cfg DatabaseConfig) *Database {
		t.Helper()
		db, err := OpenDatabase(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		if db.ID != bootstrapMemberID {
			t.Fatalf("bootstrap identity changed: %d", db.ID)
		}
		return db
	}
	first := open(firstConfig)
	second := open(secondConfig)
	for i, db := range []*Database{first, second} {
		if _, err := db.SQL.ExecContext(ctx, "CREATE TABLE storage_marker (value INTEGER NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.ExecContext(ctx, "INSERT INTO storage_marker VALUES (?)", i+1); err != nil {
			t.Fatal(err)
		}
	}
	check := func(db *Database, expected int) {
		t.Helper()
		var count, value int
		if err := db.SQL.QueryRowContext(ctx, "SELECT count(*), min(value) FROM storage_marker").Scan(&count, &value); err != nil || count != 1 || value != expected {
			t.Fatalf("independent marker count=%d value=%d expected=%d: %v", count, value, expected, err)
		}
		members, err := db.Members(ctx)
		if err != nil || len(members) != 1 || members[0].ID != db.ID || members[0].Address != db.Address {
			t.Fatalf("independent membership changed: %+v: %v", members, err)
		}
	}
	check(first, 1)
	check(second, 2)
	withoutCertificate := firstDial.Clone()
	withoutCertificate.Certificates = nil
	for _, transport := range []struct {
		name string
		tls  *tls.Config
	}{{name: "plaintext"}, {name: "TLS without client certificate", tls: withoutCertificate}} {
		probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
		var options []client.Option
		if transport.tls != nil {
			options = append(options, client.WithDialFunc(client.DialFuncWithTLS(client.DefaultDialFunc, transport.tls)))
		}
		probe, err := client.New(probeCtx, first.Address, options...)
		if err == nil {
			// TLS 1.3 clients may finish their handshake before observing the
			// server's missing-certificate rejection. Require a protocol reply.
			_, err = probe.Leader(probeCtx)
			_ = probe.Close()
		}
		probeCancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s did not encounter an actual transport rejection: %v", transport.name, err)
		}
	}
	check(first, 1)
	check(second, 2)
	alias := filepath.Join(t.TempDir(), "live-storage-alias")
	if err := os.Symlink(firstConfig.Dir, alias); err != nil {
		t.Fatal(err)
	}
	socket, err := databaseUnixSocket(firstConfig.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{firstConfig.Dir, alias} {
		duplicateConfig := firstConfig
		duplicateConfig.Dir = dir
		duplicate, err := StartDatabase(ctx, duplicateConfig)
		if err == nil {
			_ = duplicate.Close()
			t.Fatal("duplicate live storage owner was accepted")
		}
		// Require a storage rejection, before the TCP listener starts; an
		// occupied public port alone would not prove storage ownership. The
		// same path is refused by the database process lock. An alias reaches
		// the canonical native socket, which only one owner can bind.
		locked := dir == firstConfig.Dir && strings.Contains(err.Error(), dir+".helper.lock") && strings.Contains(err.Error(), "file lock is held by another process")
		bound := strings.Contains(err.Error(), "failed to set bind address") && strings.Contains(err.Error(), socket)
		if !locked && !bound {
			t.Fatalf("duplicate storage was not rejected by its lock or its canonical native socket: %v", err)
		}
	}
	check(first, 1)
	check(second, 2)

	// The other cluster's certificate must still be rejected at the real TLS
	// boundary, even though both endpoints have the same native bootstrap ID.
	unauthorized := databaseTLSConfig(t, firstListen, firstDial)
	unauthorized.Seeds = []string{second.Address}
	rejectCtx, rejectCancel := context.WithTimeout(ctx, 2*time.Second)
	rejected, err := OpenDatabase(rejectCtx, unauthorized)
	rejectCancel()
	if err == nil {
		_ = rejected.Close()
		t.Fatal("independent cluster's certificate was accepted")
	}
	check(first, 1)
	check(second, 2)

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing one owner must leave the other owner live and release only the
	// first socket. Restart keeps the persisted ID, address, and database.
	check(second, 2)
	startCtx, startCancel := context.WithCancel(ctx)
	restarted, err := StartDatabase(startCtx, firstConfig)
	startCancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	})
	if restarted.ID != first.ID || restarted.Address != first.Address {
		t.Fatalf("restart changed persisted identity: %d %s", restarted.ID, restarted.Address)
	}
	if err := restarted.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	check(restarted, 1)
	check(second, 2)
}

func TestDatabaseUnixSocketCanonicalStorage(t *testing.T) {
	requireDatabaseHelper(t)
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	first, err := databaseUnixSocket(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := databaseUnixSocket(alias)
	if err != nil || first != second {
		t.Fatalf("storage alias socket differs: %q %q: %v", first, second, err)
	}
	other, err := databaseUnixSocket(t.TempDir())
	if err != nil || first == other {
		t.Fatalf("independent storage shares socket %q: %v", other, err)
	}
	if !strings.HasPrefix(first, "@") || len(first) >= 108 {
		t.Fatalf("not a bounded Linux abstract Unix socket: %q", first)
	}
	if _, err := databaseUnixSocket(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing storage directory accepted")
	}
}
