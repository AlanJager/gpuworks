//go:build linux && dqlite

package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/restorehold"
)

// prepareClosedBusiness assembles business services only while the short-lived
// majority-backed candidate proof is eligible. The returned assembly borrows the closed
// App's Store and must be closed if the caller does not activate it.
// Preparing does not change either listener, start workers, or open HTTP routes.
func (a *App) prepareClosedBusiness(ctx context.Context) (*App, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || !a.joinedClosed || a.store == nil || a.enrollment == nil || a.businessHTTP == nil {
		return nil, errors.New("business preparation requires a closed member app")
	}
	eligibility := a.currentClosedMaterialEligibility()
	if eligibility == nil || !eligibility.Eligible() {
		return nil, errors.New("closed member has no current candidate proof")
	}
	// The worker proof re-reads local identity. Re-read it once more for assembly so
	// startup routes use the current material, never the boot-time bundle.
	bundle, _, _ := a.candidateEnrollment(ctx)
	if bundle == nil {
		return nil, errors.New("closed member enrollment changed after candidate proof")
	}
	holdScheduling, err := restorehold.SchedulingIsolationRequired(a.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.assembleBusinessGeneration(bundle, holdScheduling)
}
