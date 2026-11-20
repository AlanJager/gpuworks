package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// captureClusterGrantDiagnostics routes the store's debug summaries into a
// buffer so a test can read what was logged and what was withheld.
func captureClusterGrantDiagnostics(t *testing.T, st *Store) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	st.SetLogger(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &output
}

// catalogGCUnknownCommitPool commits for real and then reports failure, the
// shape of a commit whose acknowledgment was lost.
type catalogGCUnknownCommitPool struct {
	*sql.DB
	failure error
}

func (p *catalogGCUnknownCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &catalogGCUnknownCommitTx{Tx: tx, failure: p.failure}, nil
}

type catalogGCUnknownCommitTx struct {
	*sql.Tx
	failure error
}

func (tx *catalogGCUnknownCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return tx.failure
}

func membershipFixture(t *testing.T) (*Store, ClusterIdentity, []ClusterAdmission) {
	t.Helper()
	st, id, admin := admissionFixture(t)
	seed, err := st.InitializeClusterSeedAdmission(t.Context(), id.ClusterID, strings.Repeat("c", 64), "seed.example:9010")
	if err != nil {
		t.Fatal(err)
	}
	admissions := []ClusterAdmission{seed}
	for i := range 2 {
		_, token, err := st.IssueClusterInvitation(t.Context(), id.ClusterID, admin, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		peer, err := st.ClaimClusterInvitation(t.Context(), id.ClusterID, token, strings.Repeat(string(rune('d'+i)), 64), fmt.Sprintf("peer-%d.example:9010", i))
		if err != nil {
			t.Fatal(err)
		}
		admissions = append(admissions, peer)
	}
	return st, id, admissions
}
