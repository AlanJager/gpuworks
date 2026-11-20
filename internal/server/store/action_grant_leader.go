package store

import (
	"context"
	"errors"
)

// ObservedActionLeader is a live Raft observation, not a quorum proof by
// itself. Callers must bracket it with majority-confirmed replicated writes.
func (s *Store) ObservedActionLeader(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.currentClusterLeader == nil {
		return 0, errors.New("live action leader observation unavailable")
	}
	id, err := s.currentClusterLeader(ctx)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, errors.New("live action leader has no identity")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return id, nil
}
