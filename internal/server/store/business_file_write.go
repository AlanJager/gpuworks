package store

import (
	"context"

	"gorm.io/gorm"
)

// withBusinessFileWrite holds admission through synchronous work and cleanup.
// Its short phase transaction completes before file I/O. Subsequent checks
// inspect only the context and local eligibility; they perform no database I/O.
func (s *Store) withBusinessFileWrite(ctx context.Context, work func(check func() error) error) error {
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
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b != nil {
			return b.check(ctx)
		}
		return nil
	}
	if err := s.withBusinessWriteTx(ctx, func(*gorm.DB) error { return nil }); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return work(check)
}
