package cluster

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDatabaseLocalQuorumSnapshotPreservesSQLFactsAndJoinsBeforeB(t *testing.T) {
	db, wire, path, before := newSnapshotDatabase(t, snapshotPacket(21, false), snapshotPacket(22, true))
	var heldTx *sql.Tx
	var reads int
	wire.beforeDial = func(round int) error {
		if round == 2 {
			if wire.sqlFactories.Load() != 1 || wire.sqlCleanups.Load() != 1 {
				return errors.New("B was sent before snapshot SQL factory cleanup returned")
			}
			if heldTx == nil {
				return errors.New("B was sent before SQL callback")
			}
			if err := heldTx.QueryRow("SELECT id FROM facts").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
				return errors.New("B was sent before actual transaction rollback")
			}
			if db.Scheduling.Stats().InUse != 0 {
				return errors.New("B was sent before dedicated Conn.Close")
			}
		}
		return nil
	}
	proof, err := db.WithLocalQuorumReadSnapshot(t.Context(), func(ctx context.Context, tx *sql.Tx, a QuorumProof) error {
		reads++
		heldTx = tx
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			return errors.New("snapshot lacks shared five-second lifetime")
		}
		if a.Index != 21 || a.Term != 7 || a.ConfigurationIndex != 20 || a.LeaderID != 11 || len(a.Members) != 2 {
			return errors.New("callback did not receive original A proof facts")
		}
		if db.Scheduling.Stats().InUse != 1 {
			return errors.New("callback did not own dedicated SQL connection")
		}
		wire.mu.Lock()
		closed := len(wire.rounds) == 1 && wire.rounds[0].closed.Load()
		wire.mu.Unlock()
		if !closed {
			return errors.New("SQL began before A actual connection Close")
		}
		return snapshotSelect(ctx, tx)
	})
	if err != nil || proof.Index != 22 || proof.Term != 7 || proof.ConfigurationIndex != 20 || proof.LeaderID != 11 || len(proof.Members) != 2 || reads != 1 {
		t.Fatalf("stable local snapshot failed: %#v / %v, reads=%d", proof, err, reads)
	}
	// B's member order is deliberately reversed; membership facts are identical.
	if proof.Members[0].ID != 12 || proof.Members[1].ID != 11 {
		t.Fatal("fixture did not exercise reordered B wire members", proof.Members)
	}
	wire.check(t, 2, 1)
	assertSnapshotUntouched(t, db, path, before, heldTx)
}

func TestDatabaseLocalQuorumSnapshotRejectsRemoteABeforeRead(t *testing.T) {
	for _, changed := range []string{"id", "address"} {
		t.Run(changed, func(t *testing.T) {
			db, wire, path, before := newSnapshotDatabase(t, snapshotPacket(21, false))
			if changed == "id" {
				db.ID = 12
			} else {
				db.Address = "other.example:9001"
			}
			// Occupy the only real SQL connection. Remote A must be refused
			// before attempting Conn/BeginTx, rather than waiting for this slot.
			blocker, err := db.Scheduling.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = blocker.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			var reads int
			proof, err := db.WithLocalQuorumReadSnapshot(ctx, func(context.Context, *sql.Tx, QuorumProof) error { reads++; return nil })
			if err == nil || ctx.Err() != nil || !reflect.DeepEqual(proof, QuorumProof{}) || reads != 0 {
				t.Fatalf("remote A admitted SQL reader: %#v / %v, reads=%d", proof, err, reads)
			}
			wire.check(t, 1, 0)
			if err := blocker.Close(); err != nil {
				t.Fatal(err)
			}
			assertSnapshotUntouched(t, db, path, before, nil)
		})
	}
}

func TestDatabaseLocalQuorumSnapshotRejectsChangedB(t *testing.T) {
	for _, changed := range []string{"term", "configuration", "member-role", "same-index", "older-index"} {
		t.Run(changed, func(t *testing.T) {
			b := snapshotPacket(22, false)
			switch changed {
			case "term":
				binary.LittleEndian.PutUint64(b[16:24], 8)
			case "configuration":
				binary.LittleEndian.PutUint64(b[32:40], 21)
			case "member-role":
				binary.LittleEndian.PutUint64(b[128:136], 1)
			case "same-index":
				binary.LittleEndian.PutUint64(b[24:32], 21)
			case "older-index":
				binary.LittleEndian.PutUint64(b[24:32], 20)
			}
			db, wire, path, before := newSnapshotDatabase(t, snapshotPacket(21, false), b)
			var heldTx *sql.Tx
			var reads int
			proof, err := db.WithLocalQuorumReadSnapshot(t.Context(), func(ctx context.Context, tx *sql.Tx, _ QuorumProof) error {
				reads++
				heldTx = tx
				return snapshotSelect(ctx, tx)
			})
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) || reads != 1 {
				t.Fatalf("changed B accepted: %#v / %v, reads=%d", proof, err, reads)
			}
			wire.check(t, 2, 1)
			assertSnapshotUntouched(t, db, path, before, heldTx)
		})
	}
}

func TestDatabaseLocalQuorumSnapshotCallbackFailureNeverSendsB(t *testing.T) {
	for _, changed := range []string{"reader-error", "parent-cancel"} {
		t.Run(changed, func(t *testing.T) {
			db, wire, path, before := newSnapshotDatabase(t, snapshotPacket(21, false))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			readFailure := errors.New("actual snapshot reader failed after SELECT")
			wantErr := readFailure
			if changed == "parent-cancel" {
				wantErr = context.Canceled
			}
			var heldTx *sql.Tx
			var reads int
			proof, err := db.WithLocalQuorumReadSnapshot(ctx, func(readCtx context.Context, tx *sql.Tx, _ QuorumProof) error {
				reads++
				heldTx = tx
				if err := snapshotSelect(readCtx, tx); err != nil {
					return err
				}
				if changed == "parent-cancel" {
					cancel()
					return nil
				}
				return readFailure
			})
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(proof, QuorumProof{}) || reads != 1 {
				t.Fatalf("callback failure returned proof or lost cause: %#v / %v, reads=%d", proof, err, reads)
			}
			wire.check(t, 1, 1)
			assertSnapshotUntouched(t, db, path, before, heldTx)
		})
	}
}

func TestDatabaseLocalQuorumSnapshotCloseWaitsReaderAndSQLCleanup(t *testing.T) {
	db, wire, path, before := newSnapshotDatabase(t, snapshotPacket(21, false))
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseReader := func() { releaseOnce.Do(func() { close(release) }) }
	var reads atomic.Int32
	var heldTx *sql.Tx
	readDone, closeDone := make(chan struct{}), make(chan struct{})
	result := make(chan quorumOwnerResult, 1)
	ctx, cancel := context.WithCancel(t.Context())
	closeStarted := false
	go func() {
		defer close(readDone)
		proof, err := db.WithLocalQuorumReadSnapshot(ctx, func(readCtx context.Context, tx *sql.Tx, _ QuorumProof) error {
			reads.Add(1)
			heldTx = tx
			if err := snapshotSelect(readCtx, tx); err != nil {
				close(entered)
				return err
			}
			close(entered)
			<-readCtx.Done()
			close(canceled)
			// Real SELECT completed, but its admitted reader has not acknowledged stop.
			<-release
			return readCtx.Err()
		})
		result <- quorumOwnerResult{proof, err}
	}()
	t.Cleanup(func() {
		releaseReader()
		cancel()
		if closeStarted {
			waitQuorumOwnerDone(t, closeDone)
		} else {
			db.closeQuorum()
		}
		waitQuorumOwnerDone(t, readDone)
	})
	waitQuorumOwnerDone(t, entered)
	closeStarted = true
	go func() { defer close(closeDone); db.closeQuorum() }()
	waitQuorumOwnerDone(t, canceled)
	rejected, err := db.WithLocalQuorumReadSnapshot(t.Context(), func(context.Context, *sql.Tx, QuorumProof) error {
		reads.Add(1)
		return errors.New("closed owner admitted another reader")
	})
	if err == nil || !reflect.DeepEqual(rejected, QuorumProof{}) || reads.Load() != 1 || wire.calls.Load() != 1 {
		t.Fatalf("closed owner admitted another snapshot: %#v / %v, reads=%d rounds=%d", rejected, err, reads.Load(), wire.calls.Load())
	}
	quorumOwnerStillWaiting(t, readDone)
	quorumOwnerStillWaiting(t, closeDone)
	releaseReader()
	waitQuorumOwnerDone(t, readDone)
	waitQuorumOwnerDone(t, closeDone)
	final := <-result
	if !errors.Is(final.err, context.Canceled) || !reflect.DeepEqual(final.proof, QuorumProof{}) {
		t.Fatalf("closed snapshot returned proof or lost cancel: %#v", final)
	}
	wire.check(t, 1, 1)
	assertSnapshotUntouched(t, db, path, before, heldTx)
	db.closeQuorum()
}
