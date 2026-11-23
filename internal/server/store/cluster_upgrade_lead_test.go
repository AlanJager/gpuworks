package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeLeadership behaves like the database a handover is asked of: one
// member leads, a handover is accepted or refused by that leader, and an
// accepted handover takes effect only some looks later, as an election does.
type fakeLeadership struct {
	leader      uint64
	noLeader    int     // looks that find no leader
	refusals    []error // refusals the leader gives before it accepts
	delay       int     // looks an accepted handover needs
	lose        bool    // the accepted handover never takes effect
	pending     uint64
	transfers   []uint64
	looks       int
	transferErr error
	// giveUp ends the caller's wait after that many looks, so a test of
	// the wait running out does not depend on how fast the machine is.
	giveUp     int
	cancelWait context.CancelFunc
}

// waiting returns a context that the fake ends after giveUp looks.
func (f *fakeLeadership) waiting(t *testing.T, giveUp int) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	f.giveUp, f.cancelWait = giveUp, cancel
	return ctx
}

func (f *fakeLeadership) Leader(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.looks++
	if f.giveUp > 0 && f.looks >= f.giveUp {
		defer f.cancelWait()
	}
	if f.noLeader > 0 {
		f.noLeader--
		return 0, errors.New("database has no leader")
	}
	if f.pending != 0 && !f.lose {
		if f.delay == 0 {
			f.leader, f.pending = f.pending, 0
		} else {
			f.delay--
		}
	}
	return f.leader, nil
}

func (f *fakeLeadership) Transfer(ctx context.Context, id uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.transfers = append(f.transfers, id)
	if len(f.refusals) > 0 {
		err := f.refusals[0]
		f.refusals = f.refusals[1:]
		return err
	}
	if f.transferErr != nil {
		return f.transferErr
	}
	f.pending = id
	return nil
}

func (f *fakeLeadership) leadership(self uint64) clusterLeadership {
	return clusterLeadership{Self: self, Leader: f.Leader, Transfer: f.Transfer, Poll: time.Millisecond}
}

func TestLeadershipHandoverAsksTheLeaderUntilThisMemberLeads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// A member that leads asks nothing.
	leading := &fakeLeadership{leader: 2}
	if err := leading.leadership(2).take(ctx); err != nil || len(leading.transfers) != 0 {
		t.Fatalf("a leading member: %v, %d handovers asked; want none", err, len(leading.transfers))
	}

	// The leader refuses twice (it is busy), then accepts, and the handover
	// takes effect three looks later.
	busy := &fakeLeadership{leader: 1, refusals: []error{errors.New("leadership transfer in progress"), errors.New("not leader")}, delay: 3}
	if err := busy.leadership(2).take(ctx); err != nil {
		t.Fatalf("handover after two refusals: %v", err)
	}
	if busy.leader != 2 || len(busy.transfers) < 3 {
		t.Fatalf("leader %d after %d requests; want member 2 after at least three", busy.leader, len(busy.transfers))
	}
	for _, id := range busy.transfers {
		if id != 2 {
			t.Fatalf("a handover was asked for member %d, not this member", id)
		}
	}

	// No leader for a while (an election is under way): the member waits
	// and asks once one answers.
	electing := &fakeLeadership{leader: 3, noLeader: 4}
	if err := electing.leadership(2).take(ctx); err != nil || electing.leader != 2 {
		t.Fatalf("handover after an election: %v, leader %d", err, electing.leader)
	}
}

func TestLeadershipHandoverGivesUpWithTheLastRefusal(t *testing.T) {
	// The leader refuses every request: the end of the caller's wait ends
	// the handover and the error names the refusal, not only the wait.
	refused := &fakeLeadership{leader: 1, transferErr: errors.New("target member is not a voter")}
	err := refused.leadership(2).take(refused.waiting(t, 4))
	if err == nil || !strings.Contains(err.Error(), "target member is not a voter") || !errors.Is(err, context.Canceled) {
		t.Fatalf("a refused handover = %v; want the refusal and the end of the wait", err)
	}
	if refused.leader != 1 || len(refused.transfers) < 3 {
		t.Fatalf("leader %d after %d requests; want the leader kept and the request repeated", refused.leader, len(refused.transfers))
	}

	// The leader accepts but the handover never takes effect.
	lost := &fakeLeadership{leader: 1, lose: true}
	err = lost.leadership(2).take(lost.waiting(t, 4))
	if err == nil || !strings.Contains(err.Error(), "did not come to lead") {
		t.Fatalf("an accepted handover that never took effect = %v", err)
	}

	// No leader at all until the wait ends.
	none := &fakeLeadership{noLeader: 1 << 30}
	err = none.leadership(2).take(none.waiting(t, 4))
	if err == nil || !strings.Contains(err.Error(), "database has no leader") || len(none.transfers) != 0 {
		t.Fatalf("no leader = %v with %d handovers asked", err, len(none.transfers))
	}

	// A wait that has already ended asks nothing.
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	late := &fakeLeadership{leader: 1}
	if err := late.leadership(2).take(ended); !errors.Is(err, context.Canceled) || len(late.transfers) != 0 {
		t.Fatalf("a wait that had ended = %v with %d handovers asked", err, len(late.transfers))
	}

	// A member without an identity or a database cannot ask.
	if err := (clusterLeadership{}).take(t.Context()); err == nil {
		t.Fatal("a member without an identity asked for the leadership")
	}
}

func TestLeadershipIsHandedOverOnlyToAMemberWhoseBusinessCanBeOpen(t *testing.T) {
	serving := ClusterUpgradeStatus{Phase: MaintenancePhaseState{Phase: MaintenanceServing}, Applied: 5, Known: 5}
	if err := refuseClusterLead(serving); err != nil {
		t.Fatalf("a serving member on the cluster's schema was refused: %v", err)
	}
	ahead := serving
	ahead.Known = 6
	if err := refuseClusterLead(ahead); !errors.Is(err, ErrClusterUpgradeRefused) || !strings.Contains(err.Error(), "1 migrations ahead") {
		t.Fatalf("a member ahead of the schema = %v; want a refusal", err)
	}
	extension := serving
	extension.KnownExtensions, extension.AppliedExtensions = 3, 2
	if err := refuseClusterLead(extension); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("a member whose extension is ahead of the schema = %v; want a refusal", err)
	}
	for _, phase := range []string{MaintenanceFreezing, MaintenanceFrozen, MaintenanceUpgrading} {
		closed := serving
		closed.Phase.Phase = phase
		if err := refuseClusterLead(closed); !errors.Is(err, ErrClusterUpgradeRefused) || !strings.Contains(err.Error(), phase) {
			t.Fatalf("phase %s = %v; want a refusal that names it", phase, err)
		}
	}
}
