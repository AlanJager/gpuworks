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
	// Poll is the pause between two looks at who leads; zero means 50
	// milliseconds. Backoff is the pause after the first refused request,
	// doubled with every further refusal up to ten times its value; zero
	// means 100 milliseconds. Now is the clock; nil means time.Now.
	Poll, Backoff time.Duration
	Now           func() time.Time
}

// take makes Self the database leader and returns once Self observes that it
// leads. Who leads is looked at every Poll. The request itself opens a
// connection to the leader, so it is sent sparingly: again after an accepted
// one only when a second has passed without effect, and after a refusal (the
// leader is busy with another change, or has just changed itself) with a
// pause that doubles. When ctx ends the error carries the last refusal. A
// member that already leads returns at once and asks nothing.
func (l clusterLeadership) take(ctx context.Context) error {
	if l.Self == 0 || l.Leader == nil || l.Transfer == nil {
		return errors.New("this member cannot ask for the database leadership")
	}
	poll, backoff, now := l.Poll, l.Backoff, l.Now
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	if now == nil {
		now = time.Now
	}
	const accepted = time.Second
	pause, limit := backoff, 10*backoff
	var next time.Time
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
		case !now().Before(next):
			err := l.Transfer(ctx, l.Self)
			switch {
			case err == nil:
				next, pause = now().Add(accepted), backoff
			case ctx.Err() == nil:
				last = err
				next = now().Add(pause)
				pause = min(2*pause, limit)
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

// leadClusterUpgradeStep is the lead step once the caller is admitted: the
// refusal comes first, so that a member that leads a cluster which does not
// serve is told so rather than answered as a success; a member that already
// leads then asks nothing; otherwise it takes the leadership. It reports
// whether the member leads afterwards.
func leadClusterUpgradeStep(ctx context.Context, status func(context.Context) (ClusterUpgradeStatus, error), leads bool, take func(context.Context) error) (ClusterUpgradeStatus, bool, error) {
	observed, err := status(ctx)
	if err != nil {
		return observed, leads, err
	}
	if err := refuseClusterLead(observed); err != nil {
		return observed, leads, err
	}
	if leads {
		return observed, true, nil
	}
	if err := take(ctx); err != nil {
		return observed, false, err
	}
	return observed, true, nil
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
