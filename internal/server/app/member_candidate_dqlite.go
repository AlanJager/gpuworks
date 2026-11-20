//go:build linux && dqlite

package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// newCandidateMaterialEligibility is the closed member's request-path
// material gate input. The business assembly owner must run it with the App
// lifetime and call Invalidate on observed membership or material changes.
// Constructing it does not open any business route by itself.
func newCandidateMaterialEligibility(a *App) *materialEligibility {
	log := slog.Default()
	if a != nil && a.log != nil {
		log = a.log
	}
	prove := func(ctx context.Context) (bool, error) {
		ready, metrics, err := a.CandidateMemberReadyMeasured(ctx)
		log.DebugContext(ctx, "candidate material proof completed", "ready", ready, "err_class", closedMemberErrorClass(err),
			"failure_stage", metrics.FailureStage, "failure_class", metrics.FailureClass,
			"total_ms", float64(metrics.Total.Duration)/float64(time.Millisecond), "metrics", metrics)
		return ready, err
	}
	eligibility := newMaterialEligibility(time.Now, prove, materialReproofInterval)
	eligibility.setIdleRest(materialIdleReproofRest)
	return eligibility
}

// CandidateMemberReady proves that this closed App is currently admitted and
// has the canonical material bytes needed by the live cluster. This is an
// on-demand, potentially expensive network and disk proof; callers must bound
// their context and must not invoke it on every business request.
func (a *App) CandidateMemberReady(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if a == nil || !a.joinedClosed || a.legacyClosed || a.store == nil {
		return false, nil
	}
	bundle, memberID, key, cause := a.candidateEnrollmentWithCause(ctx)
	if bundle == nil {
		return materialReadinessFailure(ctx, cause)
	}
	// Membership is established by the material checker against a fresh live
	// snapshot. The route supplies only the local member ID, never authority.
	ready, err := checkJoinedStoreMaterialReadiness(ctx, a.store, bundle, memberID, a.cfg, a.log)
	if err != nil || !ready {
		return ready, err
	}
	current, currentID, currentKey, cause := a.candidateEnrollmentWithCause(ctx)
	if current == nil {
		return materialReadinessFailure(ctx, cause)
	}
	if currentID != memberID || currentKey != key || current.info != bundle.info {
		return materialReadinessFailure(ctx)
	}
	return true, nil
}

// CandidateMemberReadyMeasured runs the same on-demand proof and returns its
// aggregate cost even when readiness fails, without retaining metrics.
func (a *App) CandidateMemberReadyMeasured(ctx context.Context) (bool, CandidateProofMetrics, error) {
	recorder := &candidateProofRecorder{}
	ctx = context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
	started := time.Now()
	ready, err := a.CandidateMemberReady(ctx)
	metrics := recorder.snapshot()
	metrics.Total = CandidateProofStage{Calls: 1, Duration: time.Since(started)}
	return ready, metrics, err
}

// candidateEnrollment re-reads the local identity and its seed or persisted
// joined provenance. No cached route or earlier startup proof is authority.
func (a *App) candidateEnrollment(ctx context.Context) (*enrollmentBundle, uint64, string) {
	bundle, id, key, _ := a.candidateEnrollmentWithCause(ctx)
	return bundle, id, key
}

// candidateEnrollmentWithCause also returns a cluster identity read failure.
// One that fails while the database is between leaders says nothing about
// this member; the proof reports it as an interruption, not a negative.
func (a *App) candidateEnrollmentWithCause(ctx context.Context) (*enrollmentBundle, uint64, string, error) {
	started := candidateProofStart(ctx)
	defer candidateProofObserve(ctx, proofEnrollment, started, 0)
	begin, ok := a.store.ClusterSessionRoute()
	if !ok || begin.ID == 0 || ctx.Err() != nil {
		return nil, 0, "", nil
	}
	bundle, err := readEnrollmentBundle(a.cfg.DataDir, false)
	if err != nil || bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil {
		return nil, 0, "", nil
	}
	if err := verifyEnrollmentState(ctx, a.store, bundle); err != nil {
		return nil, 0, "", err
	}
	key, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return nil, 0, "", nil
	}
	root, err := filepath.EvalSymlinks(a.cfg.DataDir)
	if err != nil {
		return nil, 0, "", nil
	}
	_, err = os.Lstat(filepath.Join(root, "gpuworks.db.join"))
	switch {
	case err == nil:
		if err := verifyPersistedJoinedEnrollment(a.cfg, bundle); err != nil {
			return nil, 0, "", nil
		}
	case os.IsNotExist(err):
		admission, err := a.store.ClusterAdmissionForVerifiedKey(ctx, bundle.info.ClusterID, key)
		if err != nil || admission.InvitationHash != "" || admission.RevokedAt != nil {
			return nil, 0, "", nil
		}
	default:
		return nil, 0, "", nil
	}
	end, ok := a.store.ClusterSessionRoute()
	if !ok || end.ID != begin.ID {
		return nil, 0, "", nil
	}
	return bundle, begin.ID, key, nil
}
