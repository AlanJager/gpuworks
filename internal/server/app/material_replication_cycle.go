//go:build linux && dqlite

package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// replicateMaterialCycle verifies installed state anew only between mandatory
// installations. It restores no business authority and never shares a grant.
func (a *App) replicateMaterialCycle(ctx context.Context, bundle *enrollmentBundle, sourceID uint64, sourceKey string, expected store.ClusterIdentity, members []store.ClusterTransportMember, installedDigest string, sameMembers, forceInstall bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var rejected error
	if sameMembers && installedDigest != "" && !forceInstall {
		pointer, err := checkMaterialReplicationRestart(ctx, a.cfg.DataDir, a.store, bundle, sourceID, sourceKey, expected, members)
		if canceled := ctx.Err(); canceled != nil {
			if materialCycleHasLiveCause(err) {
				if materialReplicationFailureInvalidates(err) {
					a.invalidateCandidateMaterialEligibility(closeMaterialCheck)
				}
				return errors.Join(err, canceled)
			}
			return canceled
		}
		if err == nil && pointer.Digest == installedDigest {
			return nil
		}
		if err == nil {
			err = errors.New("periodic material check observed a different installed manifest")
		}
		if materialReplicationFailureInvalidates(err) {
			// Repairing the bytes cannot erase this failure or revive the
			// previous proof. A new readiness proof must establish authority.
			a.invalidateCandidateMaterialEligibility(closeMaterialCheck)
			rejected = err
		}
	}
	if err := ctx.Err(); err != nil {
		if rejected != nil {
			return errors.Join(rejected, err)
		}
		return err
	}
	err := replicateRequiredMaterialsWithYield(ctx, a.cfg.DataDir, a.store, bundle, installedDigest, forceInstall, func() { a.invalidateCandidateMaterialEligibility(closeMaterialInstalled) }, a.log, a.yieldWarmMaterialBatch)
	if rejected != nil {
		return errors.Join(rejected, err)
	}
	return err
}

// Context cancellation may coincide with an independently observed live failure.
// Only an error tree containing exclusively context leaves is pure cancellation.
func materialCycleHasLiveCause(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if materialCycleHasLiveCause(cause) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return materialCycleHasLiveCause(wrapped.Unwrap())
	}
	return err != context.Canceled && err != context.DeadlineExceeded
}
