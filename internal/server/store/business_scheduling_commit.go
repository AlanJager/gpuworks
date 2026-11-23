package store

import (
	"context"

	"gorm.io/gorm"
)

// commitBusinessSchedulingDecision adds revocable service admission to a
// management target that already requires the scheduling generation. Hold
// admission through the scheduling transaction's actual commit or rollback.
// Background scheduling and maintenance keep their own admission contracts.
func (s *Store) commitBusinessSchedulingDecision(ctx context.Context, fn func(*gorm.DB) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	b := s.businessWriteBarrier
	if b != nil {
		release, err := b.acquire()
		if err != nil {
			return err
		}
		defer release()
	}
	return s.commitSchedulingDecision(ctx, func(tx *gorm.DB) error {
		if b != nil {
			if err := b.check(ctx); err != nil {
				return err
			}
		}
		if err := requireBusinessServingMaintenancePhase(ctx, tx); err != nil {
			return err
		}
		if err := s.requireBusinessWriterAdmitted(ctx, tx); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		if err := readBusinessServingMaintenancePhase(ctx, tx); err != nil {
			return err
		}
		if b != nil {
			return b.check(ctx)
		}
		return nil
	})
}
