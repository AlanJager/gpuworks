package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// servingClusterWriteTransaction serializes a material grant with phase
// changes on the replicated writer. The zero-row update only acquires the
// writer; callers still need a real committed mutation for quorum proof.
// Administrative maintenance transitions deliberately use other transactions.
func (s *Store) servingClusterWriteTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	ctx, endWork := s.bindMaintenanceWork(ctx, MaintenanceWorkTransaction)
	defer endWork()
	// What a member records while serving (material replicas, receipts and
	// manifests, the action-grant roster, its enrollment origin) is a
	// member's write like any other: a revoked member does not commit it.
	return s.clusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		phase, err := (&Store{db: tx}).ReadMaintenancePhase(ctx)
		if err != nil {
			return err
		}
		if err := requireServingClusterPhase(phase); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return requireUnchangedServingClusterPhase(tx, ctx, phase)
	})
}

func requireServingClusterPhase(phase MaintenancePhaseState) error {
	if phase.Phase != MaintenanceServing {
		// A non-serving phase closes business startup the same way a local
		// barrier does. Callers match both sentinels.
		return errors.Join(ErrBusinessWritesClosed, fmt.Errorf("%w: %s at revision %d", ErrMaintenanceNotServing, phase.Phase, phase.Revision))
	}
	return nil
}

func requireUnchangedServingClusterPhase(tx *gorm.DB, ctx context.Context, phase MaintenancePhaseState) error {
	after, err := (&Store{db: tx}).ReadMaintenancePhase(ctx)
	if err != nil {
		return err
	}
	if err := requireServingClusterPhase(after); err != nil {
		return err
	}
	if after != phase {
		return ErrMaintenancePhaseChanged
	}
	return ctx.Err()
}
