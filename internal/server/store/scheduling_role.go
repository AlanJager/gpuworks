package store

import (
	"context"
	"errors"
)

// SetLocalSchedulingLeader installs the role source LocalSchedulingLeader reads.
// The native runtime reports the database's own leader. A nil source leaves the
// previous source in place. A single-process store starts consulting the source
// instead of always answering local.
func (s *Store) SetLocalSchedulingLeader(leader func(context.Context) (bool, error)) {
	if leader == nil {
		return
	}
	if s.schedulingDB == nil {
		s.schedulingDB = s.db
	}
	s.localSchedulingLeader = leader
}

// LocalSchedulingLeader observes where the database leader currently lives.
// It is a controller lifecycle hint, not a lease, quorum proof, or authority to
// commit work later. Scheduling decisions still require WithSchedulingTx.
func (s *Store) LocalSchedulingLeader(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.schedulingDB == nil {
		return true, nil
	}
	if s.localSchedulingLeader == nil {
		return false, errors.New("replicated store has no local scheduling role observer")
	}
	local, err := s.localSchedulingLeader(ctx)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return local, nil
}

// rejectKnownRemoteSchedulingLeader is an early rejection hint for scheduling
// mutations. A follower's strict local dqlite writer may retry opening a
// connection for a long time. A positive observation is never authority: the
// transaction still checks its generation before and after the mutation.
func (s *Store) rejectKnownRemoteSchedulingLeader(ctx context.Context) error {
	if s.schedulingDB == nil || s.localSchedulingLeader == nil {
		return nil
	}
	local, err := s.LocalSchedulingLeader(ctx)
	if err != nil {
		return err
	}
	if !local {
		return ErrStaleSchedulingGeneration
	}
	return nil
}
