package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

var remoteSnapshotMember = Member{ID: 11, Address: "node.example:9001", Role: "voter"}

// Real SQLite transactions and literal proof pipes establish Go ordering;
// native SQL transport, remote C drain and majority qualification are separate.
type remoteSnapshotFixture struct {
	wire     *snapshotWireFixture
	pool     *sql.DB
	observer *sql.DB
	path     string
	before   []byte
}

func newRemoteSnapshotFixture(t *testing.T, packets ...[]byte) *remoteSnapshotFixture {
	t.Helper()
	f := &remoteSnapshotFixture{wire: newSnapshotWireFixture(t, packets...), path: filepath.Join(t.TempDir(), "snapshot.db")}
	var err error
	f.observer, err = sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.observer.Close() })
	for _, query := range []string{"CREATE TABLE facts (id INTEGER PRIMARY KEY, value TEXT NOT NULL)", "INSERT INTO facts VALUES (7, 'original-fact')"} {
		if _, err := f.observer.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	f.before, err = os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.pool, err = sql.Open("sqlite", "file:"+f.path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	f.pool.SetMaxOpenConns(1)
	f.pool.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = f.pool.Close() })
	return f
}

func (f *remoteSnapshotFixture) open(ctx context.Context, member Member, name string, _ func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, error) {
	if member != remoteSnapshotMember || name != "snapshot.db" {
		return nil, nil, errors.New("SQL opener lost exact remote member or database name")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f.wire.sqlFactories.Add(1)
	return f.pool, func() error {
		err := f.pool.Close()
		f.wire.sqlCleanups.Add(1)
		return err
	}, nil
}

func (f *remoteSnapshotFixture) unchanged(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if tx != nil {
		if err := tx.QueryRow("SELECT id FROM facts").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("transaction escaped snapshot cleanup", err)
		}
	}
	// database/sql rolls a transaction back on its own goroutine when the
	// context ends, so the connection may return to the pool just after the
	// read has returned.
	for deadline := time.Now().Add(5 * time.Second); f.pool.Stats().InUse != 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if f.pool.Stats().InUse != 0 {
		t.Fatal("dedicated SQL connection remains in use")
	}
	var value string
	if err := f.observer.QueryRow("SELECT value FROM facts WHERE id = 7").Scan(&value); err != nil || value != "original-fact" {
		t.Fatalf("original row changed: %q %v", value, err)
	}
	after, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(after, f.before) {
		t.Fatal("remote snapshot changed SQLite file", err)
	}
}

func TestRemoteExistingSnapshotOrdersActualSQLCleanupBeforeB(t *testing.T) {
	f := newRemoteSnapshotFixture(t, snapshotPacket(21, false), snapshotPacket(22, true))
	var tx *sql.Tx
	f.wire.beforeDial = func(round int) error {
		if round == 2 {
			if f.wire.sqlCleanups.Load() != 1 || tx == nil || f.pool.Stats().InUse != 0 {
				return errors.New("B preceded actual SQL cleanup")
			}
			if err := tx.QueryRow("SELECT id FROM facts").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
				return errors.New("B preceded transaction rollback")
			}
			if err := f.pool.Ping(); err == nil {
				return errors.New("B preceded actual SQL pool close")
			}
		}
		return nil
	}
	proof, err := readRemoteExistingReadSnapshot(t.Context(), remoteSnapshotMember, "snapshot.db", f.wire.dial, func(ctx context.Context, actual *sql.Tx, first QuorumProof) error {
		tx = actual
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || first.Index != 21 {
			return errors.New("reader lost the shared attempt context or A")
		}
		f.wire.mu.Lock()
		closed := len(f.wire.rounds) == 1 && f.wire.rounds[0].closed.Load()
		f.wire.mu.Unlock()
		if !closed {
			return errors.New("SQL began before A actual Close")
		}
		// The callback receives a clone; modifying it cannot rewrite A.
		first.Members[0].Address = "changed.example:9001"
		return snapshotSelect(ctx, actual)
	}, f.open)
	if err != nil || proof.Index != 22 || len(proof.Members) != 2 || proof.Members[0].ID != 12 {
		t.Fatalf("remote stable snapshot failed: %+v %v", proof, err)
	}
	f.wire.check(t, 2, 1)
	f.unchanged(t, tx)
}

func TestRemoteExistingSnapshotRefusesInvalidSetupWithoutIO(t *testing.T) {
	for _, name := range []string{"zero ID", "noncanonical address", "nonvoter", "database path", "nil dial", "nil reader", "canceled"} {
		t.Run(name, func(t *testing.T) {
			member, database := remoteSnapshotMember, "snapshot.db"
			calls := 0
			dial := func(context.Context, string) (net.Conn, error) { calls++; return nil, errors.New("unexpected I/O") }
			read := func(context.Context, *sql.Tx, QuorumProof) error { calls++; return errors.New("unexpected read") }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "zero ID":
				member.ID = 0
			case "noncanonical address":
				member.Address = "NODE.example:9001"
			case "nonvoter":
				member.Role = "spare"
			case "database path":
				database = "../snapshot.db"
			case "nil dial":
				dial = nil
			case "nil reader":
				read = nil
			case "canceled":
				cancel()
			}
			proof, err := ReadRemoteExistingReadSnapshot(ctx, member, database, dial, read)
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) || calls != 0 {
				t.Fatalf("invalid setup performed I/O or exposed proof: %+v %v calls=%d", proof, err, calls)
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("parent cancellation cause lost", err)
			}
		})
	}
}

func TestRemoteExistingSnapshotRefusesChangedAWithoutSQL(t *testing.T) {
	for _, name := range []string{"leader ID", "leader address", "invalid barrier"} {
		t.Run(name, func(t *testing.T) {
			packet := snapshotPacket(21, false)
			switch name {
			case "leader ID":
				binary.LittleEndian.PutUint64(packet[40:48], 12)
				binary.LittleEndian.PutUint64(packet[128:136], 0)
			case "leader address":
				packet = bytes.Replace(packet, []byte("node.example"), []byte("evil.example"), 1)
			case "invalid barrier":
				binary.LittleEndian.PutUint64(packet[16:24], 0)
			}
			f := newRemoteSnapshotFixture(t, packet)
			reads := 0
			proof, err := readRemoteExistingReadSnapshot(t.Context(), remoteSnapshotMember, "snapshot.db", f.wire.dial, func(context.Context, *sql.Tx, QuorumProof) error { reads++; return nil }, f.open)
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) || reads != 0 {
				t.Fatalf("mismatched A admitted SQL: %+v %v reads=%d", proof, err, reads)
			}
			f.wire.check(t, 1, 0)
			f.unchanged(t, nil)
		})
	}
}

func TestRemoteExistingSnapshotRefusesChangedB(t *testing.T) {
	for _, name := range []string{"term", "configuration", "leader", "address", "member ID", "member role", "member removed", "same index", "older index"} {
		t.Run(name, func(t *testing.T) {
			packet := snapshotPacket(22, false)
			switch name {
			case "term":
				binary.LittleEndian.PutUint64(packet[16:24], 8)
			case "configuration":
				binary.LittleEndian.PutUint64(packet[32:40], 21)
			case "leader":
				binary.LittleEndian.PutUint64(packet[40:48], 12)
				binary.LittleEndian.PutUint64(packet[128:136], 0)
			case "address":
				packet = bytes.Replace(packet, []byte("node.example"), []byte("evil.example"), 1)
			case "member ID":
				binary.LittleEndian.PutUint64(packet[96:104], 13)
			case "member role":
				binary.LittleEndian.PutUint64(packet[128:136], 1)
			case "member removed":
				packet = packet[:96]
				binary.LittleEndian.PutUint64(packet[48:56], 1)
				binary.LittleEndian.PutUint32(packet[:4], uint32((len(packet)-8)/8))
			case "same index":
				binary.LittleEndian.PutUint64(packet[24:32], 21)
			case "older index":
				binary.LittleEndian.PutUint64(packet[24:32], 20)
			}
			f := newRemoteSnapshotFixture(t, snapshotPacket(21, false), packet)
			var tx *sql.Tx
			proof, err := readRemoteExistingReadSnapshot(t.Context(), remoteSnapshotMember, "snapshot.db", f.wire.dial, func(ctx context.Context, actual *sql.Tx, _ QuorumProof) error {
				tx = actual
				return snapshotSelect(ctx, actual)
			}, f.open)
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) || tx == nil {
				t.Fatalf("changed B exposed proof: %+v %v", proof, err)
			}
			f.wire.check(t, 2, 1)
			f.unchanged(t, tx)
		})
	}
}

func TestRemoteExistingSnapshotSQLFailuresPreventB(t *testing.T) {
	for _, name := range []string{"open", "reader", "missing table", "cleanup", "parent cancel"} {
		t.Run(name, func(t *testing.T) {
			f := newRemoteSnapshotFixture(t, snapshotPacket(21, false))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("actual SQL stage failure")
			open := f.open
			if name == "open" || name == "cleanup" {
				open = func(ctx context.Context, member Member, nameOfDB string, dial func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, error) {
					pool, cleanup, err := f.open(ctx, member, nameOfDB, dial)
					if name == "open" {
						return pool, cleanup, failure
					}
					return pool, func() error { return errors.Join(cleanup(), failure) }, err
				}
			}
			var tx *sql.Tx
			proof, err := readRemoteExistingReadSnapshot(ctx, remoteSnapshotMember, "snapshot.db", f.wire.dial, func(ctx context.Context, actual *sql.Tx, _ QuorumProof) error {
				tx = actual
				if err := snapshotSelect(ctx, actual); err != nil {
					return err
				}
				switch name {
				case "reader":
					return failure
				case "missing table":
					return actual.QueryRowContext(ctx, "SELECT id FROM absent_table").Scan(new(int))
				case "parent cancel":
					cancel()
				}
				return nil
			}, open)
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
				t.Fatalf("SQL failure exposed proof: %+v %v", proof, err)
			}
			if name != "missing table" && name != "parent cancel" && !errors.Is(err, failure) {
				t.Fatal("stage failure cause lost", err)
			}
			if name == "parent cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause lost", err)
			}
			f.wire.check(t, 1, 1)
			f.unchanged(t, tx)
		})
	}
}

func TestRemoteExistingSnapshotJoinsOriginalSocketClose(t *testing.T) {
	for _, name := range []string{"success", "close failure", "cancel during close"} {
		t.Run(name, func(t *testing.T) {
			f := newRemoteSnapshotFixture(t, snapshotPacket(21, false), snapshotPacket(22, false))
			client, peer := net.Pipe()
			held := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{})}
			failure := errors.New("actual original close failed")
			if name == "close failure" {
				held.failure = failure
			}
			var once sync.Once
			release := func() { once.Do(func() { close(held.release) }) }
			t.Cleanup(func() { release(); _ = peer.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ownerLife, ownerCancel := context.WithCancel(ctx)
			owner := &quorumSQLSockets{life: ownerLife, cancel: ownerCancel}
			t.Cleanup(func() { _ = owner.Close() })
			// This actual socket isolates owner acknowledgement sequencing. SQL
			// remains actual SQLite; it does not pretend to flow over this pipe.
			open := func(ctx context.Context, member Member, name string, dial func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, error) {
				pool, cleanup, err := f.open(ctx, member, name, dial)
				if err != nil {
					return pool, cleanup, err
				}
				_, err = owner.dial(ctx, member.Address, func(context.Context, string) (net.Conn, error) { return held, nil })
				return pool, func() error { return errors.Join(owner.Close(), cleanup()) }, err
			}
			done := make(chan struct{})
			result := make(chan quorumOwnerResult, 1)
			go func() {
				defer close(done)
				proof, err := readRemoteExistingReadSnapshot(ctx, remoteSnapshotMember, "snapshot.db", f.wire.dial, func(ctx context.Context, tx *sql.Tx, _ QuorumProof) error { return snapshotSelect(ctx, tx) }, open)
				result <- quorumOwnerResult{proof, err}
			}()
			t.Cleanup(func() { release(); cancel(); waitQuorumOwnerDone(t, done) })
			waitQuorumOwnerDone(t, held.entered)
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatal("original peer socket remains open", err)
			}
			if name == "cancel during close" {
				cancel()
			}
			quorumOwnerStillWaiting(t, done)
			if f.wire.calls.Load() != 1 {
				t.Fatal("B started before actual original Close acknowledgement")
			}
			release()
			waitQuorumOwnerDone(t, done)
			got := <-result
			if name == "success" {
				if got.err != nil || got.proof.Index != 22 {
					t.Fatalf("joined snapshot failed: %+v", got)
				}
				f.wire.check(t, 2, 1)
			} else {
				want := failure
				if name == "cancel during close" {
					want = context.Canceled
				}
				if !errors.Is(got.err, want) || !reflect.DeepEqual(got.proof, QuorumProof{}) {
					t.Fatalf("failed close/cancellation exposed proof: %+v", got)
				}
				f.wire.check(t, 1, 1)
			}
			if held.calls.Load() != 1 {
				t.Fatal("original socket was closed multiple times")
			}
			f.unchanged(t, nil)
		})
	}
}

func TestRemoteExistingSnapshotHonorsShorterParentDeadline(t *testing.T) {
	f := newRemoteSnapshotFixture(t, snapshotPacket(21, false))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	proof, err := readRemoteExistingReadSnapshot(ctx, remoteSnapshotMember, "snapshot.db", f.wire.dial, func(ctx context.Context, tx *sql.Tx, _ QuorumProof) error {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(parentDeadline) {
			return errors.New("attempt extended its parent's deadline")
		}
		if err := snapshotSelect(ctx, tx); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}, f.open)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(proof, QuorumProof{}) {
		t.Fatalf("shorter parent deadline was ignored: %+v %v", proof, err)
	}
	f.wire.check(t, 1, 1)
	f.unchanged(t, nil)
}
