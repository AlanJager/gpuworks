//go:build linux && dqlite

package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// materialStartupFailure retains a live rejection while allowing the original
// repair. Cancellation, including withdrawal from invalidation, stops repair.
func (a *App) materialStartupFailure(ctx context.Context, err error) (error, bool) {
	var rejected error
	live := materialCycleHasLiveCause(err)
	if _, joined := store.JoinedBranches(err); errors.Is(err, errMaterialSourceUnpublished) && !joined && ctx.Err() == nil {
		// Nothing was published to check against. The full replication that
		// follows is this member's first; carrying a veto to its end would
		// close the business a fresh proof opened in the meantime. A second
		// cause joined to it keeps its own verdict.
		return nil, false
	}
	if live && materialReplicationFailureInvalidates(err) {
		a.invalidateCandidateMaterialEligibility()
		rejected = err
	}
	if canceled := ctx.Err(); canceled != nil {
		if live {
			return errors.Join(err, canceled), true
		}
		return canceled, true
	}
	return rejected, false
}

// Without a startup veto, preserve both error identity and its classification.
func materialStartupResult(rejected, err error) error {
	if rejected == nil {
		return err
	}
	return errors.Join(rejected, err)
}
