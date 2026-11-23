package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// requireBusinessServingMaintenancePhase runs inside the already owned target
// transaction. The zero-row update acquires the writer before reading phase,
// serializing the target with maintenance CAS; it is not quorum proof. Empty
// cluster identity tables are valid for ordinary single-member stores.
func requireBusinessServingMaintenancePhase(ctx context.Context, tx *gorm.DB) error {
	if err := tx.Exec("UPDATE cluster_identities SET id = id WHERE 0").Error; err != nil {
		return errors.Join(ErrBusinessWritesClosed, err)
	}
	return readBusinessServingMaintenancePhase(ctx, tx)
}

// readBusinessServingMaintenancePhase re-checks the phase inside a transaction
// that already acquired the writer. A second zero-row update is not another
// admission.
func readBusinessServingMaintenancePhase(ctx context.Context, tx *gorm.DB) error {
	if err := (&Store{db: tx}).RequireServingMaintenancePhase(ctx); err != nil {
		return errors.Join(ErrBusinessWritesClosed, err)
	}
	return nil
}

// requireBusinessWriterAdmitted refuses a business write of a cluster member
// whose admission is gone or revoked. It reads the member's own admission
// inside the transaction that writes, after that transaction took the writer:
// the answer is the leader's and commits or fails with the write, so it is
// never older than the write and costs no replicated round of its own.
// Serving requests may rest on a confirmation that is seconds old; committing
// does not. A store that is not a cluster member has no key and no check.
func (s *Store) requireBusinessWriterAdmitted(ctx context.Context, tx *gorm.DB) error {
	if s == nil || s.businessWriterKey == "" {
		return nil
	}
	var admitted int64
	if err := tx.WithContext(ctx).Raw("SELECT count(*) FROM cluster_admissions WHERE key_fingerprint = ? AND revoked_at IS NULL", s.businessWriterKey).Scan(&admitted).Error; err != nil {
		return errors.Join(ErrBusinessWritesClosed, err)
	}
	if admitted == 0 {
		return errors.Join(ErrBusinessWritesClosed, ErrClusterAdmissionRejected)
	}
	return nil
}
