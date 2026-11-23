//go:build linux && dqlite

package app

import (
	"context"
	"time"
)

// materialReplicationPassFailed ends a replication pass that returned err
// while this member keeps running. Only a verdict closes the member; a pass
// that ran out of time or found no database leader stays pending, and the
// next pass repeats it.
func (a *App) materialReplicationPassFailed(err error) {
	closes := materialReplicationFailureInvalidates(err)
	if closes {
		a.invalidateCandidateMaterialEligibility()
	}
	if a.log != nil {
		a.log.Warn("cluster material replication pending", "err", err, "closes_member", closes)
	}
}

func waitForMaterialReplicationCycle(ctx context.Context, tick <-chan time.Time, interval time.Duration, failed bool) bool {
	if ctx.Err() != nil {
		return false
	}
	if failed {
		// The next iteration observes current identity and membership and
		// performs a new complete cycle. This wait never repeats a grant.
		retry := time.NewTimer(min(interval, materialReproofInterval))
		defer retry.Stop()
		tick = retry.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return ctx.Err() == nil
	}
}
