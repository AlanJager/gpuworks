//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/filelock"
)

type conversionTestCA struct{ transport *testTransport }

func newConversionTestCA(t *testing.T) conversionTestCA {
	t.Helper()
	return conversionTestCA{newTestTransport(t)}
}

// config admits a new member of this authority on a free port of host.
func (ca conversionTestCA) config(t *testing.T, dir, host string, _ int64) DatabaseConfig {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := DatabaseConfig{Dir: dir, Address: address, Name: "gpuworks.db"}
	ca.transport.admit(t, host).apply(&cfg)
	return cfg
}

func conversionSource(t *testing.T, ctx context.Context, parent string) (string, Member) {
	t.Helper()
	path := filepath.Join(parent, "gpuworks.db")
	db, err := OpenSingleDatabase(ctx, path+".dqlite", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE resources (id INTEGER PRIMARY KEY AUTOINCREMENT, uuid TEXT, token_hash TEXT, created_at TEXT, data BLOB)`,
		`CREATE INDEX resource_identity ON resources(uuid)`,
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations VALUES (17)`,
		`INSERT INTO resources VALUES (1, 'uuid-before', 'token-hash-before', '2026-01-02T03:04:05Z', X'006162FF')`,
		`INSERT INTO resources (id) VALUES (1000)`,
		`DELETE FROM resources WHERE id=1000`,
	} {
		if _, err := db.SQL.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	member := Member{ID: db.ID, Address: db.Address, Role: "voter"}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, member
}

func checkConversionRows(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	var uuid, token, created string
	var data []byte
	if err := db.SQL.QueryRowContext(ctx, `SELECT uuid, token_hash, created_at, data FROM resources WHERE id=1`).Scan(&uuid, &token, &created, &data); err != nil {
		t.Fatal(err)
	}
	if uuid != "uuid-before" || token != "token-hash-before" || created != "2026-01-02T03:04:05Z" || !bytes.Equal(data, []byte{0, 'a', 'b', 255}) {
		t.Fatalf("converted row changed: %q %q %q %x", uuid, token, created, data)
	}
	var version int
	if err := db.SQL.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 17 {
		t.Fatalf("migration marker = %d, %v", version, err)
	}
}

func conversionWait(t *testing.T, ctx context.Context, description string, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func conversionMemberHost(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() && ip.IsGlobalUnicast() {
			return ip.String()
		}
	}
	// A non-loopback network address is part of this test's actual claim.
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatal("native address-conversion test requires a non-loopback IPv4 address")
	}
	t.Skip("native address-conversion test requires a non-loopback IPv4 address")
	return ""
}

func TestSingleAddressConversionThreeVotersAndRecovery(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	root := t.TempDir()
	source, original := conversionSource(t, ctx, root)
	before, err := inspectConversionDirectory(ctx, source+".dqlite")
	if err != nil {
		t.Fatal(err)
	}
	ca := newConversionTestCA(t)
	host := conversionMemberHost(t)
	config := ca.config(t, filepath.Join(root, "converted"), host, 2)
	backup := filepath.Join(root, "cold-backup")
	converted, err := PrepareSingleMemberAddress(ctx, source, backup, config)
	if err != nil {
		t.Fatal(err)
	}
	if converted.ID != original.ID || converted.Address != config.Address || converted.Role != "voter" {
		t.Fatalf("converted identity = %+v, original = %+v", converted, original)
	}
	if opened, err := OpenSingleDatabase(ctx, config.Dir, config.Name); err == nil {
		_ = opened.Close()
		t.Fatal("ordinary single-member runtime bypassed the new member's TLS configuration")
	}
	for _, dir := range []string{source + ".dqlite", filepath.Join(backup, "database")} {
		after, err := inspectConversionDirectory(ctx, dir)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("source/cold backup differs at %s: %v", dir, err)
		}
	}
	// The candidate is closed on return, so the unchanged source can actually
	// be reopened at its old address as a rollback. Never run both concurrently.
	fallback, err := OpenSingleDatabase(ctx, source+".dqlite", config.Name)
	if err != nil {
		t.Fatalf("original source cannot reopen for rollback: %v", err)
	}
	checkConversionRows(t, ctx, fallback)
	if fallback.ID != original.ID || fallback.Address != original.Address {
		t.Fatal("source identity changed")
	}
	if err := fallback.Close(); err != nil {
		t.Fatal(err)
	}
	open := func(cfg DatabaseConfig) *Database {
		t.Helper()
		db, err := OpenDatabase(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	a := open(config)
	checkConversionRows(t, ctx, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = open(config)
	checkConversionRows(t, ctx, a)
	bc := ca.config(t, filepath.Join(root, "member-b"), host, 3)
	cc := ca.config(t, filepath.Join(root, "member-c"), host, 4)
	bc.Seeds, cc.Seeds = []string{config.Address}, []string{config.Address}
	identities := []DatabaseConfig{config, bc, cc}
	for index, member := range identities {
		for _, peer := range identities[index+1:] {
			if bytes.Equal(member.ListenTLS.Certificates[0].Certificate[0], peer.ListenTLS.Certificates[0].Certificate[0]) || reflect.DeepEqual(member.ListenTLS.Certificates[0].PrivateKey, peer.ListenTLS.Certificates[0].PrivateKey) {
				t.Fatal("member certificates or private keys were shared")
			}
		}
	}
	b, c := open(bc), open(cc)
	conversionWait(t, ctx, "three real voters", func() bool {
		members, err := a.Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	checkConversionRows(t, ctx, b)
	checkConversionRows(t, ctx, c)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	conversionWait(t, ctx, "surviving two members can write", func() bool {
		_, err := b.SQL.ExecContext(ctx, `INSERT INTO resources (id, uuid) VALUES (1001, 'after-one-failure') ON CONFLICT(id) DO UPDATE SET uuid=excluded.uuid`)
		return err == nil
	})
	checkConversionRows(t, ctx, c)
	var value string
	if err := c.SQL.QueryRowContext(ctx, `SELECT uuid FROM resources WHERE id=1001`).Scan(&value); err != nil || value != "after-one-failure" {
		t.Fatalf("surviving read = %q, %v", value, err)
	}
	members, err := b.Members(ctx)
	if err != nil || len(members) != 3 {
		t.Fatalf("membership changed after minority loss: %+v, %v", members, err)
	}
	for _, member := range members {
		if member.Role != "voter" {
			t.Fatalf("membership was demoted after minority loss: %+v", members)
		}
	}
	a = open(config)
	if a.ID != original.ID {
		t.Fatal("converted member restart changed ID")
	}
	checkConversionRows(t, ctx, a)
	conversionWait(t, ctx, "restarted member can assume leadership", func() bool {
		leader, err := a.Leader(ctx)
		if err == nil && leader.ID == a.ID {
			return true
		}
		_ = b.Transfer(ctx, a.ID)
		return false
	})
	// Use the local-only pool after transfer so a forwarded read cannot conceal
	// a restarted replica that has not recovered the acknowledged write.
	if err := a.Scheduling.QueryRowContext(ctx, `SELECT uuid FROM resources WHERE id=1001`).Scan(&value); err != nil || value != "after-one-failure" {
		t.Fatalf("restarted member did not retain new committed value: %q, %v", value, err)
	}
	insert, err := a.Scheduling.ExecContext(ctx, `INSERT INTO resources (uuid) VALUES ('next-sequence')`)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := insert.LastInsertId()
	if err != nil || inserted != 1002 {
		t.Fatalf("autoincrement was not preserved: %d, %v", inserted, err)
	}
}

func TestSingleAddressConversionRefusesUnsafeInputs(t *testing.T) {
	requireDatabaseHelper(t)
	for _, name := range []string{"running source", "runtime lock", "existing target", "existing backup", "overlapping output", "canceled", "bad TLS", "missing TLS", "bad target address", "missing identity", "joining source"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			root := t.TempDir()
			source, _ := conversionSource(t, ctx, root)
			config := newConversionTestCA(t).config(t, filepath.Join(root, "candidate"), "127.0.0.1", 2)
			backup := filepath.Join(root, "backup")
			switch name {
			case "running source":
				db, err := OpenSingleDatabase(ctx, source+".dqlite", config.Name)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
			case "runtime lock":
				release, err := filelock.Acquire(source + ".runtime.lock")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "existing target":
				if err := os.Mkdir(config.Dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config.Dir, "sentinel"), []byte("keep target"), 0600); err != nil {
					t.Fatal(err)
				}
			case "existing backup":
				if err := os.Mkdir(backup, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(backup, "sentinel"), []byte("keep backup"), 0600); err != nil {
					t.Fatal(err)
				}
			case "overlapping output":
				config.Dir = filepath.Join(source+".dqlite", "candidate")
			case "canceled":
				cancel()
			case "bad TLS":
				other := newConversionTestCA(t).config(t, filepath.Join(root, "unused"), "127.0.0.1", 3)
				config.DialTLS = other.DialTLS
			case "missing TLS":
				config.ListenTLS, config.DialTLS = nil, nil
			case "bad target address":
				config.Address = "0.0.0.0:19000"
			case "missing identity":
				if err := os.Remove(filepath.Join(source+".dqlite", "info.yaml")); err != nil {
					t.Fatal(err)
				}
			case "joining source":
				if err := os.WriteFile(filepath.Join(source+".dqlite", "join"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := inspectConversionDirectory(context.Background(), source+".dqlite")
			if err != nil {
				t.Fatal(err)
			}
			operationCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			_, conversionErr := PrepareSingleMemberAddress(operationCtx, source, backup, config)
			if conversionErr == nil {
				t.Fatal("unsafe conversion accepted")
			}
			if name != "running source" {
				after, err := inspectConversionDirectory(context.Background(), source+".dqlite")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("failed conversion mutated the source: %v", err)
				}
			}
			if name == "bad TLS" {
				if !errors.Is(conversionErr, context.DeadlineExceeded) {
					t.Fatalf("expected bounded cancellation during TLS validation: %v", conversionErr)
				}
				if _, err := os.Stat(filepath.Join(backup, "manifest.json")); err != nil {
					t.Fatalf("TLS failure discarded cold backup: %v", err)
				}
				if _, err := os.Stat(config.Dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("TLS failure published a candidate: %v", err)
				}
				saved, err := inspectConversionDirectory(context.Background(), filepath.Join(backup, "database"))
				if err != nil || !reflect.DeepEqual(before, saved) {
					t.Fatalf("TLS failure damaged the complete cold backup: %v", err)
				}
			}
			if name == "existing target" || name == "existing backup" {
				dir, want := config.Dir, "keep target"
				if name == "existing backup" {
					dir, want = backup, "keep backup"
				}
				got, err := os.ReadFile(filepath.Join(dir, "sentinel"))
				if err != nil || string(got) != want {
					t.Fatalf("existing output overwritten: %q, %v", got, err)
				}
			}
		})
	}
}

func TestSingleAddressConversionRejectsMisleadingMemberCache(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	root := t.TempDir()
	source, identity := conversionSource(t, ctx, root)
	a, err := OpenDatabase(ctx, DatabaseConfig{Dir: source + ".dqlite", Address: identity.Address, Name: "gpuworks.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	peers := []*Database{a}
	for index := 0; index < 2; index++ {
		cfg := databaseTLSConfig(t, nil, nil)
		cfg.Seeds = []string{a.Address}
		peer, err := OpenDatabase(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = peer.Close() })
		peers = append(peers, peer)
	}
	conversionWait(t, ctx, "three source voters", func() bool {
		members, err := a.Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	for _, db := range peers {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt only the discovery cache; actual persisted membership has three
	// voters. The converter must not use this cache to force a singleton.
	raw, err := yaml.Marshal([]client.NodeInfo{{ID: identity.ID, Address: identity.Address, Role: client.Voter}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source+".dqlite", "cluster.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := inspectConversionDirectory(ctx, source+".dqlite")
	if err != nil {
		t.Fatal(err)
	}
	config := newConversionTestCA(t).config(t, filepath.Join(root, "candidate"), "127.0.0.1", 4)
	_, err = PrepareSingleMemberAddress(ctx, source, filepath.Join(root, "backup"), config)
	if err == nil || !strings.Contains(err.Error(), "actual Raft members") {
		t.Fatalf("did not reject authoritative multi-member configuration: %v", err)
	}
	after, err := inspectConversionDirectory(ctx, source+".dqlite")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected multi-member source changed: %v", err)
	}
	if _, err := os.Stat(config.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("multi-member source published a singleton: %v", err)
	}
}
