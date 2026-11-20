//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// The same test is explicitly run against the preserved older native prefix
// with mode=unsupported. No fake peer substitutes for that compatibility run.
func TestNativeExistingReadSQL(t *testing.T) {
	requireDatabaseHelper(t)
	mode := os.Getenv("GPUWORKS_TEST_EXISTING_READ_MODE")
	if mode == "" {
		mode = "supported"
	}
	if mode != "supported" && mode != "unsupported" {
		t.Fatal("unknown existing-read test mode", mode)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 1)
	member := fixtures[0]
	if err := identity.PublishTransportPolicy(member.policyPath, 0, policy); err != nil {
		t.Fatal(err)
	}
	db := openAuthorizedFixture(t, ctx, trust, member)
	materialQuorumSeedSQL(t, ctx, db.SQL, fixtures)
	baseline := materialQuorumSQLSnapshot(t, ctx, db.SQL)
	dial := materialQuorumTLS(t, trust, policy, member)

	t.Run("missing-database-never-registers-or-creates", func(t *testing.T) {
		const missing = "existing-read-missing.db"
		assertMissing := func() {
			peer, err := client.New(ctx, db.Address, client.WithDialFunc(dial))
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			files, err := peer.Dump(ctx, missing)
			if err == nil || len(files) != 0 {
				t.Fatalf("missing database registered or materialized: files=%+v err=%v", files, err)
			}
		}
		assertMissing()
		pool, cleanup, observed := nativeExistingReadPool(t, ctx, db.ID, db.Address, missing, dial)
		var value int
		if err := pool.QueryRowContext(ctx, "SELECT 1").Scan(&value); err == nil {
			t.Fatal("strict read opened a missing database")
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		assertNativeExistingReadWire(t, observed)
		assertMissing()
		if got := materialQuorumSQLSnapshot(t, ctx, db.SQL); !reflect.DeepEqual(got, baseline) {
			t.Fatal("missing-database refusal changed schema/rows/markers/sequence/audit")
		}
	})

	if mode == "unsupported" {
		t.Run("actual-old-native-refuses-without-fallback", func(t *testing.T) {
			pool, cleanup, observed := nativeExistingReadPool(t, ctx, db.ID, db.Address, member.config.Name, dial)
			var value string
			if err := pool.QueryRowContext(ctx, "SELECT uuid FROM quorum_resources WHERE id=1").Scan(&value); err == nil {
				t.Fatal("older native accepted strict OPEN")
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			assertNativeExistingReadWire(t, observed)
			if got := materialQuorumSQLSnapshot(t, ctx, db.SQL); !reflect.DeepEqual(got, baseline) {
				t.Fatal("old native refusal changed original SQL")
			}
		})
		return
	}

	t.Run("native-rejects-invalid-open-before-reading", func(t *testing.T) {
		for _, mode := range []string{"flags", "vfs", "path", "oversized-name", "trailing-bytes", "schema"} {
			t.Run(mode, func(t *testing.T) {
				name, vfs, flags, schema := member.config.Name, "volatile", uint64(0), byte(0)
				switch mode {
				case "flags":
					flags = 1
				case "vfs":
					vfs = "other"
				case "path":
					name = "../" + name
				case "oversized-name":
					name = strings.Repeat("x", 512)
				case "schema":
					schema = 1
				}
				text := func(value string) []byte {
					result := make([]byte, (len(value)+8)&^7)
					copy(result, value)
					return result
				}
				body := append(text(name), make([]byte, 8)...)
				binary.LittleEndian.PutUint64(body[len(body)-8:], flags)
				body = append(body, text(vfs)...)
				if mode == "trailing-bytes" {
					body = append(body, make([]byte, 8)...)
				}
				// Bypass the validating adapter deliberately: this must prove the
				// native endpoint itself refuses malformed strict OPEN input.
				conn, err := dial(ctx, db.Address)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				packet := make([]byte, 16)
				binary.LittleEndian.PutUint64(packet[:8], 1)
				binary.LittleEndian.PutUint32(packet[8:12], uint32(len(body)/8))
				packet[12], packet[13] = 20, schema
				packet = append(packet, body...)
				if n, err := conn.Write(packet); err != nil || n != len(packet) {
					t.Fatalf("native invalid OPEN write n=%d err=%v", n, err)
				}
				var response [8]byte
				if _, err := io.ReadFull(conn, response[:]); err != nil || response[4] != 0 || response[5] != 0 {
					t.Fatalf("native malformed OPEN did not return failure: header=%x err=%v", response, err)
				}
				words := binary.LittleEndian.Uint32(response[:4])
				if words < 2 || words > 8192 {
					t.Fatal("invalid native failure size", words)
				}
				failure := make([]byte, int(words)*8)
				if _, err := io.ReadFull(conn, failure); err != nil || binary.LittleEndian.Uint64(failure[:8]) == 0 {
					t.Fatalf("native invalid OPEN omitted its error: body=%x err=%v", failure, err)
				}
			})
		}
		if got := materialQuorumSQLSnapshot(t, ctx, db.SQL); !reflect.DeepEqual(got, baseline) {
			t.Fatal("malformed native OPEN changed original SQL")
		}
	})

	t.Run("actual-read-transaction-and-write-refusal", func(t *testing.T) {
		pool, cleanup, observed := nativeExistingReadPool(t, ctx, db.ID, db.Address, member.config.Name, dial)
		defer func() {
			if err := cleanup(); err != nil {
				t.Error(err)
			}
		}()
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var value string
		if err := tx.QueryRowContext(ctx, "SELECT uuid FROM quorum_resources WHERE id=1").Scan(&value); err != nil || value != "original-resource" {
			_ = tx.Rollback()
			t.Fatalf("strict actual row value=%q err=%v", value, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("strict transaction did not roll back", err)
		}
		for _, query := range []string{
			`INSERT INTO quorum_resources(uuid) VALUES('must-not-exist')`,
			`UPDATE schema_migrations SET version=999`,
			`CREATE TABLE must_not_exist(id INTEGER)`,
		} {
			if _, err := conn.ExecContext(ctx, query); err == nil {
				t.Fatal("read-only native connection accepted write", query)
			}
		}
		if err := conn.QueryRowContext(ctx, "SELECT uuid FROM quorum_resources WHERE id=1").Scan(&value); err != nil || value != "original-resource" {
			t.Fatalf("write refusal damaged readable connection: value=%q err=%v", value, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		assertNativeExistingReadWire(t, observed)
		if got := materialQuorumSQLSnapshot(t, ctx, db.SQL); !reflect.DeepEqual(got, baseline) {
			t.Fatal("strict reads or refused writes changed original SQL")
		}
	})
	t.Run("native-restart-restores-existing-registry", func(t *testing.T) {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db = openAuthorizedFixture(t, ctx, trust, member)
		pool, cleanup, observed := nativeExistingReadPool(t, ctx, db.ID, db.Address, member.config.Name, dial)
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT count(*) FROM quorum_resources WHERE id=1 AND uuid='original-resource'").Scan(&count); err != nil || count != 1 {
			_ = cleanup()
			t.Fatalf("restored existing read count=%d err=%v", count, err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		assertNativeExistingReadWire(t, observed)
		if got := materialQuorumSQLSnapshot(t, ctx, db.SQL); !reflect.DeepEqual(got, baseline) {
			t.Fatal("strict read after native replay changed original SQL")
		}
	})
}

type nativeExistingReadWire struct {
	net.Conn
	mu   sync.Mutex
	data []byte
}

func (c *nativeExistingReadWire) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.data = append(c.data, p[:n]...)
	c.mu.Unlock()
	return n, err
}

func nativeExistingReadPool(t *testing.T, ctx context.Context, id uint64, address, name string, dial func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, *[]*nativeExistingReadWire) {
	t.Helper()
	var mu sync.Mutex
	observed := new([]*nativeExistingReadWire)
	pool, cleanup, err := OpenRemoteExistingReadSQL(ctx, Member{ID: id, Address: address, Role: "voter"}, name, func(ctx context.Context, target string) (net.Conn, error) {
		conn, err := dial(ctx, target)
		if conn == nil {
			return nil, err
		}
		wire := &nativeExistingReadWire{Conn: conn}
		mu.Lock()
		*observed = append(*observed, wire)
		mu.Unlock()
		return wire, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	return pool, cleanup, observed
}

func assertNativeExistingReadWire(t *testing.T, observed *[]*nativeExistingReadWire) {
	t.Helper()
	strict := 0
	for _, wire := range *observed {
		wire.mu.Lock()
		data := append([]byte(nil), wire.data...)
		wire.mu.Unlock()
		if len(data) < 8 || binary.LittleEndian.Uint64(data[:8]) != 1 {
			t.Fatal("actual TLS SQL connection omitted protocol v1")
		}
		for data = data[8:]; len(data) != 0; {
			if len(data) < 8 {
				t.Fatal("actual SQL request left partial header")
			}
			size := 8 + int(binary.LittleEndian.Uint32(data[:4]))*8
			if size > len(data) {
				t.Fatal("actual SQL request left partial body")
			}
			if data[4] == 3 {
				t.Fatal("strict SDK path sent a creating OPEN")
			}
			if data[4] == 20 {
				strict++
			}
			data = data[size:]
		}
	}
	if strict == 0 {
		t.Fatal("no actual strict native OPEN request observed")
	}
}
