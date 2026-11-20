//go:build linux && dqlite

package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The seam changes only the acknowledgement AFTER a real native commit, or
// performs a real pointer write at that exact boundary. It cannot authorize a
// failed transaction, fake membership, or replace the production grant.
type candidateGrantCommitKey struct{}
type candidateGrantCommitPool struct {
	*sql.DB
	calls atomic.Int64
	at    int64
	after func() error
}

func (p *candidateGrantCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &candidateGrantCommitTx{Tx: tx, pool: p, measured: ctx.Value(candidateGrantCommitKey{}) == p}, nil
}

type candidateGrantCommitTx struct {
	*sql.Tx
	pool     *candidateGrantCommitPool
	measured bool
	grant    bool
}

func (tx *candidateGrantCommitTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "UPDATE") && strings.Contains(query, "material_grant_sequence") {
		tx.grant = true
	}
	return tx.Tx.ExecContext(ctx, query, args...)
}
func (tx *candidateGrantCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.measured && tx.grant && tx.pool.calls.Add(1) == tx.pool.at {
		return tx.pool.after()
	}
	return nil
}

func TestNativeCandidateFinalUnknownCommitDoesNotAuthorize(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	var before store.ClusterAdmission
	db := f.stores[0].DB()
	if err := db.Where("key_fingerprint = ?", f.keys[0]).First(&before).Error; err != nil {
		t.Fatal(err)
	}
	lost := errors.New("injected lost final commit acknowledgement")
	var injected atomic.Bool
	pool := &candidateGrantCommitPool{DB: f.databases[0].SQL, at: 2, after: func() error { injected.Store(true); return lost }}
	previous := db.Statement.ConnPool
	db.Statement.ConnPool = pool
	defer func() { db.Statement.ConnPool = previous }()
	ready, metrics, err := f.prove(context.WithValue(ctx, candidateGrantCommitKey{}, pool))
	db.Statement.ConnPool = previous
	if ready || err != nil || metrics.MembershipGrant.Calls != 2 || pool.calls.Load() != 2 || !injected.Load() {
		t.Fatalf("unknown final grant accepted/not reached: ready=%t grants=%d commits=%d injected=%t err=%v", ready, metrics.MembershipGrant.Calls, pool.calls.Load(), injected.Load(), err)
	}
	var after store.ClusterAdmission
	if err := db.Where("key_fingerprint = ?", f.keys[0]).First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.MaterialGrantSequence <= before.MaterialGrantSequence {
		t.Fatal("unknown-ack fixture did not actually commit")
	}
}

func TestNativePublicManifestVerifierRetainsFreshGrantsAndFinalPointerOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	db := f.stores[0].DB()
	source := f.pointers[0]
	var before store.ClusterAdmission
	if err := db.Where("key_fingerprint = ?", source.SourceKey).First(&before).Error; err != nil {
		t.Fatal(err)
	}
	recorder := &candidateProofRecorder{}
	measured := context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
	if _, err := verifyReceivedMaterialManifest(measured, f.stores[0], f.receivers[0], f.expected(), source.SourceKey, source.Digest); err != nil {
		t.Fatal(err)
	}
	var after store.ClusterAdmission
	if err := db.Where("key_fingerprint = ?", source.SourceKey).First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if recorder.snapshot().MembershipGrant.Calls != 2 || after.MaterialGrantSequence-before.MaterialGrantSequence != 2 {
		t.Fatal("public verifier no longer has two independent fresh source grants")
	}
	var injected atomic.Bool
	pool := &candidateGrantCommitPool{DB: f.databases[0].SQL, at: 2, after: func() error {
		injected.Store(true)
		_, err := f.databases[0].SQL.ExecContext(ctx, "UPDATE material_manifest_records SET recorded_at = ? WHERE source_member_id = ?", time.Now().UTC().Add(time.Hour), source.SourceMemberID)
		return err
	}}
	previous := db.Statement.ConnPool
	db.Statement.ConnPool = pool
	defer func() { db.Statement.ConnPool = previous }()
	_, err := verifyReceivedMaterialManifest(context.WithValue(ctx, candidateGrantCommitKey{}, pool), f.stores[0], f.receivers[0], f.expected(), source.SourceKey, source.Digest)
	db.Statement.ConnPool = previous
	if !injected.Load() || pool.calls.Load() != 2 || err == nil || !strings.Contains(err.Error(), "pointer changed") {
		t.Fatalf("public verifier read latest pointer before final grant: injected=%t commits=%d err=%v", injected.Load(), pool.calls.Load(), err)
	}
}
