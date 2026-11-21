//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestNativeLocalQuorumReadSnapshot(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 3)
	dbs := make([]*Database, len(fixtures))
	for i := range fixtures {
		if i > 0 {
			fixtures[i].config.Seeds = []string{fixtures[0].config.Address}
		}
		if err := identity.PublishTransportPolicy(fixtures[i].policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
		dbs[i] = openAuthorizedFixture(t, ctx, trust, fixtures[i])
	}
	conversionWait(t, ctx, "snapshot actual three-voter configuration", func() bool {
		members, err := dbs[0].Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	initial := settledQuorumProof(t, ctx, "snapshot first proof", dbs[0])
	var leader, follower *Database
	for _, db := range dbs {
		if db.ID == initial.LeaderID {
			leader = db
		} else {
			follower = db
		}
	}
	if leader == nil || follower == nil {
		t.Fatal("actual native leader/follower not found")
	}
	materialQuorumSeedSQL(t, ctx, leader.SQL, fixtures)
	originalSQL := materialQuorumSQLSnapshot(t, ctx, leader.SQL)

	t.Run("leader-reads-actual-local-tx-between-fresh-proofs", func(t *testing.T) {
		var before QuorumProof
		var actualTx *sql.Tx
		reads := 0
		after, err := leader.WithLocalQuorumReadSnapshot(ctx, func(readCtx context.Context, tx *sql.Tx, proof QuorumProof) error {
			reads++
			before, actualTx = proof, tx
			return nativeSnapshotReadRows(readCtx, tx)
		})
		if err != nil {
			t.Fatal("actual local snapshot", err)
		}
		materialQuorumAssertProof(t, before, initial.Members)
		materialQuorumAssertProof(t, after, initial.Members)
		if reads != 1 || before.LeaderID != leader.ID || after.LeaderID != leader.ID || after.Term != before.Term || after.ConfigurationIndex != before.ConfigurationIndex || after.Index <= before.Index {
			t.Fatalf("local snapshot did not preserve fresh leader/config interval: reads=%d A=%+v B=%+v", reads, before, after)
		}
		if err := actualTx.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("snapshot returned before actual transaction rollback: %v", err)
		}
		if got := materialQuorumSQLSnapshot(t, ctx, leader.SQL); !reflect.DeepEqual(got, originalSQL) {
			t.Fatal("native read snapshot changed representative SQL/schema/markers/sequence/audit")
		}
		t.Logf("LOCAL_SNAPSHOT_ACTUAL_ROWS A=%+v B=%+v TX_DONE=true", before, after)
	})
	t.Run("actual-follower-refuses-without-reader", func(t *testing.T) {
		reads := 0
		proof, err := follower.WithLocalQuorumReadSnapshot(ctx, func(context.Context, *sql.Tx, QuorumProof) error {
			reads++
			return errors.New("follower reader must not execute")
		})
		if err == nil || reads != 0 || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("follower snapshot borrowed remote leadership: reads=%d proof=%+v err=%v", reads, proof, err)
		}
		t.Logf("ACTUAL_FOLLOWER_REFUSED id=%d leader=%d err=%v", follower.ID, leader.ID, err)
	})
	t.Run("actual-transfer-during-reader-invalidates-result", func(t *testing.T) {
		var before QuorumProof
		var transferErr error
		reads := 0
		proof, err := leader.WithLocalQuorumReadSnapshot(ctx, func(readCtx context.Context, tx *sql.Tx, value QuorumProof) error {
			reads++
			before = value
			if err := nativeSnapshotReadRows(readCtx, tx); err != nil {
				return err
			}
			transferErr = leader.Transfer(readCtx, follower.ID)
			return transferErr
		})
		if reads != 1 || transferErr != nil {
			t.Fatalf("actual transfer did not occur after local SQL read: reads=%d transfer=%v snapshot=%v", reads, transferErr, err)
		}
		if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("snapshot authorized across actual leadership transfer: A=%+v returned=%+v err=%v", before, proof, err)
		}
		actualLeader, leaderErr := follower.Leader(ctx)
		if leaderErr != nil || actualLeader.ID != follower.ID {
			t.Fatalf("actual leadership transfer not observable: %+v err=%v", actualLeader, leaderErr)
		}
		if got := materialQuorumSQLSnapshot(t, ctx, follower.SQL); !reflect.DeepEqual(got, originalSQL) {
			t.Fatal("actual transfer/read cleanup changed SQL fixture")
		}
		t.Logf("ACTUAL_TRANSFER_REFUSAL A=%+v newLeader=%+v err=%v", before, actualLeader, err)
		if err := follower.Transfer(ctx, leader.ID); err != nil {
			t.Fatal("restore original leader before Close test", err)
		}
		conversionWait(t, ctx, "original leader restored", func() bool {
			actual, err := leader.Leader(ctx)
			return err == nil && actual.ID == leader.ID
		})
	})
	t.Run("actual-close-cancels-and-joins-held-reader", func(t *testing.T) {
		entered := make(chan error, 1)
		canceled := make(chan struct{})
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		returnedProof := make(chan QuorumProof, 1)
		returnedErr := make(chan error, 1)
		closed := make(chan error, 1)
		var actualTx *sql.Tx
		go func() {
			proof, err := leader.WithLocalQuorumReadSnapshot(ctx, func(readCtx context.Context, tx *sql.Tx, before QuorumProof) error {
				actualTx = tx
				readErr := nativeSnapshotReadRows(readCtx, tx)
				entered <- readErr
				if readErr != nil {
					return readErr
				}
				<-readCtx.Done()
				close(canceled)
				<-release
				return readCtx.Err()
			})
			returnedProof <- proof
			returnedErr <- err
		}()
		select {
		case err := <-entered:
			if err != nil {
				t.Fatal("actual snapshot SQL read before hold", err)
			}
		case err := <-returnedErr:
			// A leader change before this point makes the snapshot refuse at
			// once; waiting for the reader would only hide that reason.
			t.Fatal("actual snapshot returned before its reader entered", err)
		case <-ctx.Done():
			t.Fatal("actual snapshot reader never entered", ctx.Err())
		}
		select {
		case err := <-returnedErr:
			t.Fatal("actual SQL read failed before held reader boundary", err)
		default:
		}
		go func() { closed <- leader.Close() }()
		select {
		case <-canceled:
		case <-ctx.Done():
			t.Fatal("Database.Close did not cancel actual reader context", ctx.Err())
		}
		select {
		case err := <-closed:
			t.Fatalf("Close returned while reader still holds callback: %v", err)
		default:
		}
		newReads := 0
		proof, err := leader.WithLocalQuorumReadSnapshot(ctx, func(context.Context, *sql.Tx, QuorumProof) error {
			newReads++
			return nil
		})
		if err == nil || newReads != 0 || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("closing owner admitted a new snapshot: reads=%d proof=%+v err=%v", newReads, proof, err)
		}
		// Release the actual callback once. The deferred cleanup remains safe
		// on failures before this point and never opens another native owner.
		close(release)
		released = true
		select {
		case err := <-returnedErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("held native reader cancellation cause lost: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("snapshot call did not join actual read cleanup", ctx.Err())
		}
		if proof := <-returnedProof; !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("Close exposed held reader proof: %+v", proof)
		}
		if err := actualTx.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("canceled snapshot left actual transaction live: %v", err)
		}
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal("actual Database.Close", err)
			}
		case <-ctx.Done():
			t.Fatal("native owner did not close after reader cleanup", ctx.Err())
		}
		if err := leader.SQL.PingContext(ctx); err == nil {
			t.Fatal("normal SQL pool remained open after actual Close")
		}
		if err := leader.Scheduling.PingContext(ctx); err == nil {
			t.Fatal("scheduling pool remained open after actual Close")
		}
		if got := materialQuorumSQLSnapshot(t, ctx, follower.SQL); !reflect.DeepEqual(got, originalSQL) {
			t.Fatal("Close/reader cleanup changed surviving majority SQL")
		}
		t.Log("ACTUAL_CLOSE reader_ctx_canceled=true held_return_blocked=true admission_closed=true tx_done=true pools_closed=true")
	})
}

func nativeSnapshotReadRows(ctx context.Context, tx *sql.Tx) error {
	var uuid, tokenHash, data string
	if err := tx.QueryRowContext(ctx, "SELECT uuid,token_hash,hex(data) FROM quorum_resources WHERE id=1").Scan(&uuid, &tokenHash, &data); err != nil {
		return err
	}
	if uuid != "original-resource" || tokenHash != "fixture-token-hash" || data != "006162FF" {
		return fmt.Errorf("actual transaction resource changed: %q %q %q", uuid, tokenHash, data)
	}
	var marker, sequence, writes int64
	for _, query := range []struct {
		sql    string
		target *int64
	}{
		{"SELECT version FROM schema_migrations", &marker},
		{"SELECT seq FROM sqlite_sequence WHERE name='quorum_resources'", &sequence},
		{"SELECT count(*) FROM quorum_sql_writes", &writes},
	} {
		if err := tx.QueryRowContext(ctx, query.sql).Scan(query.target); err != nil {
			return err
		}
	}
	if marker != 17 || sequence != 1000 || writes != 0 {
		return fmt.Errorf("actual transaction metadata changed: marker=%d sequence=%d writes=%d", marker, sequence, writes)
	}
	return nil
}
