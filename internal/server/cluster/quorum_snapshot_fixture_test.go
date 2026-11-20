package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/glebarez/go-sqlite"
)

// The fixture speaks the literal native frame over actual pipes. It proves Go
// owner/SQL ordering only; no native majority or remote C drain is asserted.
type snapshotWireRound struct {
	client net.Conn
	peer   net.Conn
	done   chan struct{}
	err    error
	closed atomic.Bool
}

type snapshotWireClose struct {
	net.Conn
	round *snapshotWireRound
}

func (c *snapshotWireClose) Close() error {
	err := c.Conn.Close()
	c.round.closed.Store(true)
	return err
}

type snapshotWireFixture struct {
	packets      [][]byte
	calls        atomic.Int32
	sqlFactories atomic.Int32
	sqlCleanups  atomic.Int32
	mu           sync.Mutex
	rounds       []*snapshotWireRound
	beforeDial   func(int) error
}

func newSnapshotWireFixture(t *testing.T, packets ...[]byte) *snapshotWireFixture {
	t.Helper()
	f := &snapshotWireFixture{packets: packets}
	t.Cleanup(func() {
		f.mu.Lock()
		rounds := append([]*snapshotWireRound(nil), f.rounds...)
		f.mu.Unlock()
		for _, r := range rounds {
			_ = r.client.Close()
			_ = r.peer.Close()
		}
		for _, r := range rounds {
			waitQuorumOwnerDone(t, r.done)
		}
	})
	return f
}

func (f *snapshotWireFixture) dial(_ context.Context, address string) (net.Conn, error) {
	call := int(f.calls.Add(1))
	if address != "node.example:9001" {
		return nil, errors.New("unexpected snapshot discovery address")
	}
	if call > len(f.packets) {
		return nil, fmt.Errorf("unexpected quorum round %d", call)
	}
	if f.beforeDial != nil {
		if err := f.beforeDial(call); err != nil {
			return nil, err
		}
	}
	client, peer := net.Pipe()
	r := &snapshotWireRound{client: client, peer: peer, done: make(chan struct{})}
	f.mu.Lock()
	f.rounds = append(f.rounds, r)
	f.mu.Unlock()
	packet := append([]byte(nil), f.packets[call-1]...)
	go func() {
		defer close(r.done)
		defer peer.Close()
		request := make([]byte, 24)
		_, r.err = io.ReadFull(peer, request)
		want := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}
		if r.err == nil && !bytes.Equal(request, want) {
			r.err = errors.New("snapshot owner did not send exact handshake/request16/schema0/format2")
		}
		if r.err == nil {
			_, r.err = peer.Write(packet)
		}
	}()
	return &snapshotWireClose{Conn: client, round: r}, nil
}

func (f *snapshotWireFixture) check(t *testing.T, count, sqlFactories int) {
	t.Helper()
	if f.sqlFactories.Load() != int32(sqlFactories) || f.sqlCleanups.Load() != int32(sqlFactories) {
		t.Fatalf("SQL factory/cleanup calls=%d/%d, want %d", f.sqlFactories.Load(), f.sqlCleanups.Load(), sqlFactories)
	}
	if got := int(f.calls.Load()); got != count {
		t.Fatalf("quorum rounds=%d, want %d", got, count)
	}
	f.mu.Lock()
	rounds := append([]*snapshotWireRound(nil), f.rounds...)
	f.mu.Unlock()
	if len(rounds) != count {
		t.Fatalf("actual pipe rounds=%d, want %d", len(rounds), count)
	}
	for _, r := range rounds {
		waitQuorumOwnerDone(t, r.done)
		if r.err != nil {
			t.Fatal("actual snapshot wire failed", r.err)
		}
		if !r.closed.Load() {
			t.Fatal("snapshot returned before actual quorum connection Close")
		}
	}
}

// Independent literal encoding: two fixed members, with optional wire reordering.
func snapshotPacket(index uint64, reverse bool) []byte {
	packet := append([]byte(nil), quorumOwnerSuccess...)
	binary.LittleEndian.PutUint64(packet[24:32], index)
	binary.LittleEndian.PutUint64(packet[48:56], 2)
	second := []byte{12, 0, 0, 0, 0, 0, 0, 0,
		's', 'p', 'a', 'r', 'e', '.', 'e', 'x', 'a', 'm', 'p', 'l', 'e', ':', '9', '0', '0', '2', 0, 0, 0, 0, 0, 0,
		2, 0, 0, 0, 0, 0, 0, 0}
	if reverse {
		first := append([]byte(nil), packet[56:]...)
		packet = append(packet[:56], second...)
		packet = append(packet, first...)
	} else {
		packet = append(packet, second...)
	}
	binary.LittleEndian.PutUint32(packet[:4], uint32((len(packet)-8)/8))
	return packet
}

func newSnapshotDatabase(t *testing.T, packets ...[]byte) (*Database, *snapshotWireFixture, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	pool, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := pool.Exec("CREATE TABLE facts (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("INSERT INTO facts (id, value) VALUES (7, 'original-fact')"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newSnapshotWireFixture(t, packets...)
	db := &Database{ID: 11, Address: "node.example:9001", Scheduling: pool,
		leader: func(context.Context) (Member, error) {
			return Member{ID: 11, Address: "node.example:9001", Role: "voter"}, nil
		}}
	// SQLite executes real transactions; this fixture's cleanup does not own or
	// model native SQL sockets. The outer test cleanup closes its shared pool.
	bindDatabaseQuorumReader(db, f.dial, func(ctx context.Context) (*sql.DB, func() error, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		f.sqlFactories.Add(1)
		return pool, func() error { f.sqlCleanups.Add(1); return nil }, nil
	})
	t.Cleanup(db.closeQuorum)
	return db, f, path, before
}

func snapshotSelect(ctx context.Context, tx *sql.Tx) error {
	var id int
	var value, schema string
	if err := tx.QueryRowContext(ctx, "SELECT id, value FROM facts WHERE id = 7").Scan(&id, &value); err != nil {
		return err
	}
	if id != 7 || value != "original-fact" {
		return errors.New("read snapshot changed original row identity/value")
	}
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'facts'").Scan(&schema); err != nil {
		return err
	}
	if schema != "CREATE TABLE facts (id INTEGER PRIMARY KEY, value TEXT NOT NULL)" {
		return errors.New("read snapshot changed original schema")
	}
	return nil
}

func assertSnapshotUntouched(t *testing.T, db *Database, path string, before []byte, tx *sql.Tx) {
	t.Helper()
	if tx != nil {
		if err := tx.QueryRow("SELECT id FROM facts").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("transaction was not actually rolled back", err)
		}
	}
	if got := db.Scheduling.Stats().InUse; got != 0 {
		t.Fatal("dedicated SQLite connection still owned", got)
	}
	var value, schema string
	if err := db.Scheduling.QueryRow("SELECT value FROM facts WHERE id = 7").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if err := db.Scheduling.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'facts'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if value != "original-fact" || schema != "CREATE TABLE facts (id INTEGER PRIMARY KEY, value TEXT NOT NULL)" {
		t.Fatalf("original SQL facts changed: %q / %q", value, schema)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read snapshot changed SQLite file bytes")
	}
}
