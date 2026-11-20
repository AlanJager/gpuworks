//go:build linux && dqlite

package app

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// inferenceRoleObservation is the last leadership edge the controller saw.
// The proxy reads it when a new inference request starts.
type inferenceRoleObservation struct {
	mu      sync.Mutex
	seen    bool
	leader  bool
	changed bool
}

// Role observation controls lifetime only. Store scheduling transactions still
// constrain commits; observing a leader is neither a quorum grant nor an RPC fence.
func (a *App) runLeaderController(ctx context.Context, run func(context.Context)) {
	superviseController(ctx, a.store.LocalSchedulingLeader, run, a.log, time.Second, 2*time.Second, func(leader bool) error {
		if leader {
			if err := a.store.BeginSchedulingLeadership(ctx); err != nil {
				return err
			}
			a.noteInferenceRole(true)
			return nil
		}
		a.noteInferenceRole(false)
		return a.store.NoteSchedulingLeadership(ctx, false)
	}, a.restoredSchedulingOpen)
}

func (a *App) noteInferenceRole(leader bool) {
	a.inferenceRole.mu.Lock()
	defer a.inferenceRole.mu.Unlock()
	if a.inferenceRole.seen && a.inferenceRole.leader != leader {
		a.inferenceRole.changed = true
	}
	a.inferenceRole.seen = true
	a.inferenceRole.leader = leader
}

func (a *App) inferenceDatabaseRoleChanged(context.Context) bool {
	a.inferenceRole.mu.Lock()
	defer a.inferenceRole.mu.Unlock()
	changed := a.inferenceRole.changed
	a.inferenceRole.changed = false
	return changed
}

func superviseController(ctx context.Context, observe func(context.Context) (bool, error), run func(context.Context), log *slog.Logger, interval, observationTimeout time.Duration, onRole func(bool) error, schedulingOpen func() bool) {
	var cancel context.CancelFunc
	var done <-chan struct{}
	drain := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel, done = nil, nil
		}
	}
	defer drain()
	for ctx.Err() == nil {
		observation, finish := context.WithTimeout(ctx, observationTimeout)
		local, err := observe(observation)
		if err == nil {
			err = observation.Err()
		}
		finish()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Debug("observe local scheduling role", "err", err)
		}
		if !local || err != nil {
			if cancel != nil {
				log.Info("stopping controller after local scheduling role lost")
				drain()
				if onRole != nil {
					if err := onRole(false); err != nil {
						log.Debug("record scheduling leadership loss", "err", err)
					}
				}
				// Do not reuse a role observed before the old work finished draining.
				continue
			}
		} else if cancel == nil && openScheduling(schedulingOpen) {
			ready := true
			if onRole != nil {
				if err := onRole(true); err != nil {
					log.Debug("commit scheduling leadership", "err", err)
					ready = false
				}
			}
			if ready {
				child, stop := context.WithCancel(ctx)
				completed := make(chan struct{})
				cancel, done = stop, completed
				go func() { defer close(completed); run(child) }()
				log.Info("starting controller on local database leader")
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-done:
			// Unexpected controller exit is retried only after a fresh observation.
			cancel()
			cancel, done = nil, nil
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case <-timer.C:
		}
		timer.Stop()
	}
}

// openScheduling treats a missing gate as ordinary leadership.
func openScheduling(schedulingOpen func() bool) bool {
	if schedulingOpen == nil {
		return true
	}
	return schedulingOpen()
}

// schedulingAllowed keeps scheduling closed during disaster recovery until the
// old member is isolated. A cluster id change is not isolation. A nil recovery
// is ordinary leadership and is not a restore.
func schedulingAllowed(recovery func() *DisasterRecovery) bool {
	if recovery == nil {
		return true
	}
	state := recovery()
	if state == nil {
		return true
	}
	if !state.Isolated {
		return false
	}
	resumed, err := ResumeRecoveryScheduling(*state)
	if err != nil || !resumed.Scheduling {
		return false
	}
	*state = resumed
	return true
}
