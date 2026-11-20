//go:build linux && dqlite

package app

import (
	"context"
	"time"
)

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
