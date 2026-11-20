//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
)

// revocableBusiness retains every resource borrowed by one joined business
// generation until its HTTP requests, transactions, sessions, and workers drain.
type revocableBusiness interface {
	BeginStop()
	Stopping() bool
	Wait(context.Context) error
}

func (a *App) retainClosedBusiness(owner revocableBusiness) error {
	a.closedBusinessMu.Lock()
	defer a.closedBusinessMu.Unlock()
	if a.closedBusiness != nil {
		return errors.New("previous business generation is still draining")
	}
	a.closedBusiness = owner
	return nil
}

func (a *App) currentClosedBusiness() revocableBusiness {
	a.closedBusinessMu.Lock()
	defer a.closedBusinessMu.Unlock()
	return a.closedBusiness
}

func (a *App) stopClosedBusiness(ctx context.Context) error {
	owner := a.currentClosedBusiness()
	if owner == nil {
		return nil
	}
	// BeginStop withdraws routes synchronously, but it may first wait for an
	// activation phase. Let the caller's deadline cover that wait as well as
	// the subsequent request, transaction, and worker drain. The owner stays
	// retained when the deadline expires.
	go owner.BeginStop()
	err := owner.Wait(ctx)
	if err != nil && ctx.Err() != nil {
		return err
	}
	a.closedBusinessMu.Lock()
	if a.closedBusiness == owner {
		a.closedBusiness = nil
	}
	a.closedBusinessMu.Unlock()
	return err
}

// closeBusinessAdmission is safe in the material invalidation callback. It
// closes Store admission first and starts route withdrawal without waiting for
// old work, which the owner drains separately.
func (a *App) closeBusinessAdmission() {
	if a.store != nil {
		a.store.SuspendBusinessWrites()
	}

	if owner := a.currentClosedBusiness(); owner != nil {
		owner.BeginStop()
	}
	if a.log != nil && a.log.Enabled(context.Background(), slog.LevelDebug) {
		caller := "unknown"
		if pc, _, _, ok := runtime.Caller(1); ok {
			if fn := runtime.FuncForPC(pc); fn != nil {
				caller = fn.Name()
			}
		}
		a.log.Debug("closed business admission invalidated", "caller", caller)
	}
}
