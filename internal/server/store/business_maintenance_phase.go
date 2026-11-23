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
