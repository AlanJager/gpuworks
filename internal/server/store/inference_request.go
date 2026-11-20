package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// WithInferenceRequest admits a new request and retains local drain ownership
// until run returns. Callers must join actual asynchronous dial and connection
// I/O before returning. Accepted healthy work may finish after admission closes.
// No SQL transaction is held while run performs network I/O.
func (s *Store) WithInferenceRequest(ctx context.Context, run func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil {
		return errors.New("inference request callback is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b := s.businessWriteBarrier
	if b != nil {
		release, err := b.acquire()
		if err != nil {
			return err
		}
		defer release()
	}
	// This establishes the durable serving boundary for SQL fixtures and
	// plaintext single-member runtimes. It is not a quorum authorization grant.
	if err := s.withBusinessWriteTx(ctx, func(*gorm.DB) error { return nil }); err != nil {
		return err
	}
	if s.inferenceMembership != nil {
		if err := s.inferenceMembership(ctx); err != nil {
			return err
		}
	} else if s.TransportAuthorizationEnabled() || s.grantClusterMembership != nil {
		return errors.New("native inference membership owner is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b != nil {
		if err := b.check(ctx); err != nil {
			return err
		}
	}
	return run()
}
