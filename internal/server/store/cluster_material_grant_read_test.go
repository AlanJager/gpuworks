package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
)

// Count operations on the actual SQL pool rather than replacing execution or
// authorization. Native Exec/Query each sends one protocol request; the small
// result sets in this fixture fit in one response.
type materialGrantReadCountPool struct {
	*sql.DB
	mu    sync.Mutex
	calls []string
}

func (p *materialGrantReadCountPool) record(query string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, query)
}

func (p *materialGrantReadCountPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	p.record("BEGIN")
	tx, err := p.DB.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &materialGrantReadCountTx{Tx: tx, pool: p}, nil
}

type materialGrantReadCountTx struct {
	*sql.Tx
	pool *materialGrantReadCountPool
}

func (tx *materialGrantReadCountTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx.pool.record(query)
	return tx.Tx.ExecContext(ctx, query, args...)
}

func (tx *materialGrantReadCountTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	tx.pool.record(query)
	return tx.Tx.QueryContext(ctx, query, args...)
}

func (tx *materialGrantReadCountTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	tx.pool.record(query)
	return tx.Tx.QueryRowContext(ctx, query, args...)
}

func (tx *materialGrantReadCountTx) Commit() error {
	tx.pool.record("COMMIT")
	return tx.Tx.Commit()
}

func (tx *materialGrantReadCountTx) Rollback() error {
	tx.pool.record("ROLLBACK")
	return tx.Tx.Rollback()
}

func countMaterialGrantSQL(t *testing.T, st *Store, run func()) []string {
	t.Helper()
	pool, err := st.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	counted := &materialGrantReadCountPool{DB: pool}
	previous := st.db.Statement.ConnPool
	st.db.Statement.ConnPool = counted
	defer func() { st.db.Statement.ConnPool = previous }()
	run()
	return counted.calls
}

func assertMaterialGrantSQL(t *testing.T, calls []string, count int) {
	t.Helper()
	if len(calls) != count {
		t.Fatalf("actual SQL requests=%d want%d: %v", len(calls), count, calls)
	}
	if calls[0] != "BEGIN" || calls[1] != "UPDATE cluster_identities SET id = id WHERE 0" || calls[len(calls)-1] != "COMMIT" {
		t.Fatalf("grant lost writer-first acquisition or confirmed commit: %v", calls)
	}
	increments := 0
	for _, query := range calls {
		if strings.HasPrefix(query, "UPDATE `cluster_admissions`") && strings.Contains(query, "material_grant_sequence") {
			increments++
		}
	}
	if increments != 1 {
		t.Fatalf("actual distinct increment count=%d", increments)
	}
}

func TestMaterialGrantCombinedReadPreservesMissingAndErrorOrder(t *testing.T) {
	for _, fault := range []string{"missing-phase", "missing-identity", "missing-admission", "phase-before-identity", "identity-before-admission", "wrong-cluster", "wrong-ca", "wrong-protocol", "invalid-key", "revoked", "query-error", "caller-cancel"} {
		t.Run(fault, func(t *testing.T) {
			st, id, rows := membershipFixture(t)
			key := rows[1].KeyFingerprint
			ctx := t.Context()
			want := ErrClusterAdmissionRejected
			exec := func(query string, args ...any) {
				t.Helper()
				if err := st.db.Exec(query, args...).Error; err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "missing-phase":
				exec("DROP TRIGGER maintenance_phase_no_delete")
				exec("DELETE FROM maintenance_phase")
				want = ErrMaintenancePhaseUnavailable
			case "missing-identity":
				exec("DELETE FROM cluster_identities")
				want = ErrClusterNotInitialized
			case "missing-admission":
				exec("DELETE FROM cluster_admissions WHERE key_fingerprint=?", key)
			case "phase-before-identity":
				exec("UPDATE maintenance_phase SET phase='freezing',revision=revision+1")
				exec("DELETE FROM cluster_identities")
				want = ErrMaintenanceNotServing
			case "identity-before-admission":
				exec("DELETE FROM cluster_identities")
				exec("DELETE FROM cluster_admissions WHERE key_fingerprint=?", key)
				want = ErrClusterNotInitialized
			case "wrong-cluster":
				id.ClusterID = strings.Repeat("f", 32)
				want = ErrClusterIdentityConflict
			case "wrong-ca":
				id.CAFingerprint = strings.Repeat("f", 64)
				want = ErrClusterIdentityConflict
			case "wrong-protocol":
				exec("UPDATE cluster_identities SET protocol=protocol+1")
				want = ErrClusterIdentityConflict
			case "invalid-key":
				key = "invalid"
			case "revoked":
				exec("UPDATE cluster_admissions SET revoked_at=CURRENT_TIMESTAMP WHERE key_fingerprint=?", key)
			case "query-error":
				exec("ALTER TABLE cluster_admissions RENAME TO unreadable_cluster_admissions")
				want = nil
			case "caller-cancel":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
				want = context.Canceled
			}
			revision, admissions, err := st.GrantClusterTransportAdmissions(ctx, id, key)
			if err == nil || revision != 0 || admissions != nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("fault%s leaked authority or changed error: revision%d rows%v err%v want%v", fault, revision, admissions, err, want)
			}
		})
	}
}

func TestMaterialGrantCombinedReadKeepsFullAdmissionAndPrivateIdentityContract(t *testing.T) {
	st, id, rows := membershipFixture(t)
	expected := rows[1]
	expected.EnrollmentOrigin = "https://member.example:9443"
	if err := st.db.Model(&ClusterAdmission{}).Where("key_fingerprint=?", expected.KeyFingerprint).UpdateColumn("enrollment_origin", expected.EnrollmentOrigin).Error; err != nil {
		t.Fatal(err)
	}
	grant, err := st.GrantClusterMaterials(t.Context(), id.ClusterID, expected.KeyFingerprint)
	expected.MaterialGrantSequence++
	if err != nil || grant != expected {
		t.Fatalf("complete admission changed: grant%+v expected%+v err%v", grant, expected, err)
	}
	// The private-material API originally binds cluster ID and protocol; CA
	// equality is additionally required only by the transport snapshot API.
	if err := st.db.Model(&ClusterIdentity{}).Where("id=1").UpdateColumn("ca_fingerprint", strings.Repeat("f", 64)).Error; err != nil {
		t.Fatal(err)
	}
	grant, err = st.GrantClusterMaterials(t.Context(), id.ClusterID, expected.KeyFingerprint)
	expected.MaterialGrantSequence++
	if err != nil || grant != expected {
		t.Fatalf("private identity contract changed: grant%+v expected%+v err%v", grant, expected, err)
	}
	if revision, snapshot, err := st.GrantClusterTransportAdmissions(t.Context(), id, expected.KeyFingerprint); !errors.Is(err, ErrClusterIdentityConflict) || revision != 0 || snapshot != nil {
		t.Fatalf("transport omitted CA binding: revision%d rows%v err%v", revision, snapshot, err)
	}
}
