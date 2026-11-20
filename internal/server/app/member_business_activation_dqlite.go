//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"
)

func (a *App) currentClosedMaterialEligibility() *materialEligibility {
	a.materialEligibilityMu.Lock()
	defer a.materialEligibilityMu.Unlock()
	return a.materialEligibility
}

func (a *App) activateClosedBusiness(ctx context.Context) (resultErr error) {
	eligibility := a.currentClosedMaterialEligibility()
	if eligibility == nil || !eligibility.Eligible() {
		return errors.New("closed member has no current material eligibility")
	}
	staged, err := a.prepareClosedBusiness(ctx)
	if err != nil {
		return err
	}
	if a.currentClosedMaterialEligibility() != eligibility || !eligibility.Eligible() || ctx.Err() != nil {
		return errors.Join(errors.New("closed member eligibility changed during preparation"), a.discardBusinessGeneration(staged))
	}
	g := &closedBusinessGeneration{owner: a, business: staged, setupDone: make(chan struct{}), done: make(chan struct{})}
	if err := a.retainClosedBusiness(g); err != nil {
		return errors.Join(err, a.discardBusinessGeneration(staged))
	}
	defer close(g.setupDone)
	defer func() {
		if resultErr != nil {
			g.BeginStop()
		}
	}()

	g.phaseMu.Lock()
	if g.stopping {
		g.phaseMu.Unlock()
		return errors.New("business activation was withdrawn")
	}
	staged.hub.StartSessions()
	staged.proxy.Start()
	g.started = true
	g.stopSessions, err = staged.routeClusterMemberSessions(ctx, staged.memberSessions())
	g.phaseMu.Unlock()
	if err != nil {
		return err
	}
	if !eligibility.Eligible() || ctx.Err() != nil {
		return errors.New("closed member eligibility changed before route transfer")
	}
	g.phaseMu.Lock()
	if g.stopping {
		g.phaseMu.Unlock()
		return errors.New("business activation was withdrawn")
	}
	err = a.enrollment.adoptPreparedHandler(staged.enrollment)
	g.phaseMu.Unlock()
	if err != nil {
		return err
	}
	if err := a.store.OpenBusinessWrites(); err != nil {
		return err
	}
	if !eligibility.Eligible() || ctx.Err() != nil {
		return errors.New("closed member eligibility changed before worker startup")
	}
	businessCtx, cancel := context.WithCancel(ctx)
	g.phaseMu.Lock()
	if g.stopping {
		g.phaseMu.Unlock()
		cancel()
		return errors.New("business activation was withdrawn")
	}
	g.cancel = cancel
	g.workers = staged.startBusinessWorkers(businessCtx)
	g.phaseMu.Unlock()
	if !eligibility.Eligible() || ctx.Err() != nil {
		return errors.New("closed member eligibility changed before public activation")
	}
	g.phaseMu.Lock()
	if g.stopping {
		g.phaseMu.Unlock()
		return errors.New("business activation was withdrawn")
	}
	a.businessHTTP.setBusinessHandler(withinGeneration(businessCtx, staged.http.Handler))
	g.phaseMu.Unlock()
	return nil
}

func (a *App) runClosedBusiness(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		owner := a.currentClosedBusiness()
		eligibility := a.currentClosedMaterialEligibility()
		ready := eligibility != nil && eligibility.Eligible()
		switch {
		case owner != nil && (owner.Stopping() || !ready):
			shutdown, cancel := context.WithTimeout(ctx, enrollmentShutdownTimeout)
			err := a.stopClosedBusiness(shutdown)
			cancel()
			if err != nil && ctx.Err() == nil {
				a.log.Warn("closed member business generation still draining", "err", err)
			}
		case owner == nil && ready:
			err := a.activateClosedBusiness(ctx)
			if err != nil && ctx.Err() == nil {
				a.log.Warn("closed member business activation deferred", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
