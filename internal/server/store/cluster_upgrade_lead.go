package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// clusterLeadership is the part of the database a leadership handover uses:
// who leads now, and a request to the leader to hand over to a member.
type clusterLeadership struct {
	Self     uint64
	Leader   func(context.Context) (uint64, error)
	Transfer func(context.Context, uint64) error
	// Poll is the pause between two looks; zero means 50 milliseconds.
	Poll time.Duration
}

// take makes Self the database leader and returns once Self observes that it
// leads. The leader refuses a handover while it is busy with another change
// or has just changed itself, so a refusal is asked again until ctx ends; the
// error then carries the last refusal. A member that already leads returns at
// once and asks nothing.
func (l clusterLeadership) take(ctx context.Context) error {
	if l.Self == 0 || l.Leader == nil || l.Transfer == nil {
		return errors.New("this member cannot ask for the database leadership")
	}
	poll := l.Poll
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	var last error
	for {
		// An error that is only the caller's deadline says nothing about
		// the database, so it does not replace the last refusal.
		leader, err := l.Leader(ctx)
		switch {
		case err == nil && leader == l.Self:
			return nil
		case err != nil:
			if ctx.Err() == nil {
				last = err
			}
		default:
			if err := l.Transfer(ctx, l.Self); err != nil && ctx.Err() == nil {
				last = err
			}
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			if last == nil {
				last = errors.New("the leader accepted the handover but this member did not come to lead")
			}
			return fmt.Errorf("database leadership was not handed over: %w (%w)", last, ctx.Err())
		case <-timer.C:
		}
	}
}

// refuseClusterLead says why a member must not take the leadership for an
// upgrade, or nil. The member that leads serves the cluster's business, so
// only one whose own business can be open may take it: the cluster serves and
// the schema is the one this executable knows.
func refuseClusterLead(status ClusterUpgradeStatus) error {
	if status.Phase.Phase != MaintenanceServing {
		return refuseClusterUpgrade("the phase is %s; leadership is handed over only while the cluster serves", status.Phase.Phase)
	}
	if status.Pending() != 0 {
		return refuseClusterUpgrade("this member's executable is %d migrations ahead of the schema, so its business is closed and it must not lead", status.Pending())
	}
	return nil
}
