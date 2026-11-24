//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// joinedRaftSession owns the local Raft process and runtime lease until a
// classified Store takes ownership. The waiting App does not use SQL or start
// material workers before that later handoff.
type joinedRaftSession interface {
	Close() error
}

// newWaitingJoinedApp is the closed listener boundary for a persisted joined
// member. Material services are deferred until its original Raft owner has
// classified the quorum-backed schema during Run.
func newWaitingJoinedApp(runtime *memberRuntime) (*App, error) {
	cfg, log, bundle := runtime.member.cfg, runtime.member.log, runtime.bundle
	if bundle == nil {
		return nil, errors.New("joined runtime requires local enrollment materials")
	}
	if err := verifyPersistedJoinedEnrollment(cfg, bundle); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := startWaitingJoinedRaft(ctx, cfg, bundle)
	if err != nil {
		return nil, err
	}
	if candidate, ok := session.(interface{ AwaitingMembershipProof() bool }); ok && candidate.AwaitingMembershipProof() {
		if notice := joinedDatabaseListenNotice(cfg.DBPath + ".dqlite"); notice != "" {
			log.Warn("joined database will not listen", "reason", notice)
		}
	}
	a := newWaitingClosedMemberApp(runtime, session)
	if err := a.prepareClosedFactsListener(bundle); err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return a, nil
}

// Missing published HTTP coordinates leave only the public health listener.
// A prepared facts listener stays unstarted until Run owns its cleanup.
func (a *App) prepareClosedFactsListener(bundle *enrollmentBundle) error {
	if strings.TrimSpace(bundle.info.ListenAddress) == "" || strings.TrimSpace(bundle.info.PublicURL) == "" {
		return nil
	}
	server, err := newClosedFactsEnrollmentServer(bundle, a.joinedRaft, a.cfg, a.log)
	if err != nil {
		return err
	}
	a.enrollment = server
	return nil
}

// newWaitingClosedMemberApp is the App of a member whose business is closed:
// it has no store yet and serves only its closed business gate.
func newWaitingClosedMemberApp(runtime *memberRuntime, session joinedRaftSession) *App {
	member := runtime.member
	log := member.log
	if log == nil {
		log = slog.Default()
	}
	gate := newBusinessHandlerGate(log)
	gate.requireMaterialEligibility()
	a := &App{cfg: member.cfg, log: log, bus: member.events, selfLog: member.cfg.SelfLog, ha: runtime,
		http: &http.Server{Handler: gate}}
	a.joinedClosed, a.legacyClosed = true, true
	a.joinedRaft, a.businessHTTP = session, gate
	if source, ok := session.(clusterUpgradeProgressSource); ok {
		gate.upgradeProgress = newClusterUpgradeProgress(source)
	}
	return a
}

func newClosedSeedApp(runtime *memberRuntime) (*App, error) {
	cfg, bundle := runtime.member.cfg, runtime.bundle
	if bundle == nil {
		return nil, errors.New("expanded seed requires local enrollment materials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := startWaitingSeedRaft(ctx, cfg, bundle)
	if err != nil {
		return nil, err
	}
	a := newWaitingClosedMemberApp(runtime, session)
	if err := a.prepareClosedFactsListener(bundle); err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return a, nil
}

// assembleClosedMemberMaterials borrows the existing Store and public gate.
// Failure leaves Store ownership with the caller, including its runtime lease.
func (a *App) assembleClosedMemberMaterials(ctx context.Context, bundle *enrollmentBundle, joined bool) error {
	cfg, log, st := a.cfg, a.log, a.store
	if err := ctx.Err(); err != nil {
		return err
	}
	if bundle == nil {
		return errors.New("closed cluster runtime requires local enrollment materials")
	}
	if err := verifyEnrollmentState(ctx, st, bundle); err != nil {
		return err
	}
	if len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil {
		return errors.New("closed cluster runtime requires a local member certificate")
	}
	memberKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return err
	}
	if joined {
		if err := verifyPersistedJoinedEnrollment(cfg, bundle); err != nil {
			return err
		}
	} else {
		admission, err := st.ClusterAdmissionForVerifiedKey(ctx, bundle.info.ClusterID, memberKey)
		if err != nil {
			return err
		}
		if admission.InvitationHash != "" {
			return errors.New("expanded runtime local member is not the admitted seed")
		}
	}
	route, ok := st.ClusterSessionRoute()
	if !ok || route.ID == 0 {
		return errors.New("closed cluster runtime has no local member route")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	replicaDir := filepath.Join(root, "material-replicas")
	if err := os.MkdirAll(replicaDir, 0700); err != nil {
		return err
	}
	if err := syncMaterialDir(root); err != nil {
		return err
	}
	receiver, err := newMaterialReceiver(replicaDir, route.ID)
	if err != nil {
		return err
	}
	receiver.log = log
	if err := recoverOrphanedMaterialTransfers(ctx, receiver); err != nil {
		return err
	}
	receiver.onChange = a.invalidateCandidateMaterialEligibility
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	})
	requests := newMaterialRequestTracker()
	if a.joinedRaft != nil {
		facts, err := closedQuorumFactsHandler(bundle, a.joinedRaft, requests)
		if err != nil {
			return err
		}
		router.Post(enrollment.ClusterQuorumFactsPath, facts)
		discovery, err := closedQuorumDiscoveryHandler(bundle, a.joinedRaft, requests)
		if err != nil {
			return err
		}
		router.Post(enrollment.ClusterQuorumDiscoveryPath, discovery)
		if upgrade, err := clusterUpgradeHandler(bundle, a.joinedRaft, requests, a.heldAgentConnections); err == nil {
			router.Post(enrollment.ClusterUpgradePath, upgrade)
		}
	}
	registerClusterDiagnostics(router, bundle, requests, a.cfg, log)
	registerEnrollmentMaterialReceiver(router, bundle, st, receiver, requests)
	registerEnrollmentOriginWithChange(router, bundle, st, a.invalidateCandidateMaterialEligibility)
	enrollment, err := newEnrollmentServer(bundle.info.ListenAddress, router, bundle.tls, log)
	if err != nil {
		return err
	}
	if _, classified := a.joinedRaft.(joinedStoreClassifier); !classified {
		// Classified waiting owners installed the barrier before material
		// preparation. Ordinary closed-member construction installs it here.
		if err := a.installClosedBusinessWriteBarrier(); err != nil {
			return err
		}
	}
	enrollment.materialRequests = requests
	enrollment.materialReceiver = receiver
	return a.replaceClosedEnrollment(ctx, enrollment)
}

// Keep the previous owner reachable until its listener and tracked handlers
// actually finish. A timeout leaves that owner available for later cleanup.
func (a *App) replaceClosedEnrollment(ctx context.Context, prepared *enrollmentServer) error {
	old := a.enrollment
	if old != nil {
		if old.addr != prepared.addr {
			return errors.New("closed enrollment replacement changed listener address")
		}
		drain, cancel := context.WithTimeout(ctx, enrollmentShutdownTimeout)
		err := old.Close(drain)
		cancel()
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.enrollment = prepared
	return nil
}

func (a *App) runClosedJoined(ctx context.Context) error {
	return a.runClosedJoinedWithMaterialInterval(ctx, time.Minute)
}

// startClosedMaterialEligibility starts a fresh proof owner for each Run.
// It never changes the closed business handler; a later assembly step may use
// the result only after its own independent gates have been satisfied.
func (a *App) startClosedMaterialEligibility(ctx context.Context) error {
	a.materialEligibilityMu.Lock()
	defer a.materialEligibilityMu.Unlock()
	if a.materialEligibilityClosed {
		return errors.New("server app already closed")
	}
	if a.materialEligibilityDone != nil {
		select {
		case <-a.materialEligibilityDone:
			a.materialEligibility = nil
			a.materialEligibilityCancel = nil
			a.materialEligibilityDone = nil
		default:
			return errors.New("closed material eligibility still draining")
		}
	}
	eligibility := a.materialEligibility
	if eligibility == nil {
		eligibility = newCandidateMaterialEligibility(a)
	}
	if eligibility == nil {
		return nil
	}
	if a.store != nil {
		eligibility.setOnInvalidated(a.closeBusinessAdmission)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	a.materialEligibility = eligibility
	a.materialEligibilityCancel = cancel
	a.materialEligibilityDone = done
	if a.businessHTTP != nil {
		a.businessHTTP.setMaterialEligibility(eligibility)
	}
	go func() {
		defer close(done)
		eligibility.Run(workerCtx)
	}()
	return nil
}

// stopClosedMaterialEligibility cancels the proof and waits only as long as
// its caller allows. A timeout leaves the worker recorded so a later Run
// cannot overlap it and Close cannot release the Store until a retry drains it.
func (a *App) stopClosedMaterialEligibility(ctx context.Context) error {
	a.closeBusinessAdmission()
	a.materialEligibilityMu.Lock()
	eligibility := a.materialEligibility
	cancel := a.materialEligibilityCancel
	done := a.materialEligibilityDone
	if eligibility != nil {
		eligibility.Stop()
	}
	if cancel != nil {
		cancel()
	}
	a.materialEligibilityMu.Unlock()
	if done != nil {
		select {
		case <-done:
		default:
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	a.materialEligibilityMu.Lock()
	if a.materialEligibilityDone == done {
		a.materialEligibility = nil
		a.materialEligibilityCancel = nil
		a.materialEligibilityDone = nil
	}
	a.materialEligibilityMu.Unlock()
	return nil
}

// invalidateCandidateMaterialEligibility is the observation seam for future
// material or membership change notices. Invalidation closes the current
// result immediately; the background worker will perform the next full proof.
func (a *App) invalidateCandidateMaterialEligibility() {
	a.materialEligibilityMu.Lock()
	eligibility := a.materialEligibility
	a.materialEligibilityMu.Unlock()
	if eligibility != nil {
		// Latch ineligibility before BeginStop can wait on an activation phase.
		// Its callback closes Store admission and withdraws the old routes.
		eligibility.Invalidate()
	}
	a.closeBusinessAdmission()
}

// installClosedBusinessWriteBarrier keeps ordinary target writes closed until
// the business owner explicitly opens them after candidate admission. The
// predicate is deliberately local: Store calls it inside a write transaction.
func (a *App) installClosedBusinessWriteBarrier() error {
	a.materialEligibilityMu.Lock()
	if a.materialEligibility != nil {
		a.materialEligibility.setOnInvalidated(a.closeBusinessAdmission)
	}
	a.materialEligibilityMu.Unlock()
	// A write the database refused because this member is no longer admitted
	// closes the member now; its next renewal would only learn the same.
	a.store.OnWriterRefused(a.invalidateCandidateMaterialEligibility)
	return a.store.InstallBusinessWriteBarrier(func(context.Context) error {
		a.materialEligibilityMu.Lock()
		eligibility := a.materialEligibility
		a.materialEligibilityMu.Unlock()
		if eligibility == nil || !eligibility.Eligible() {
			return store.ErrBusinessWritesClosed
		}
		return nil
	})
}

// Only the closed App uses transport refresh as a candidate-readiness signal.
// An ordinary serving App still retains its existing authorization semantics.
func (a *App) closedTransportAuthorizationRefresh(refresh func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := refresh(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			return nil
		}
		// Only a verdict closes the member: a refusal the database returned,
		// an error nothing classifies, and the parent's cancellation. A
		// refresh that was interrupted, found the writer taken, ran out of
		// its cycle or got a database failure response says nothing about
		// this member, whether or not the cycle's deadline was reached. It
		// keeps the proof and the next cycle repeats it. The proof's own
		// window still ends a member that cannot verify for that long.
		ctxErr := ctx.Err()
		undecided := store.GrantUndecided.Unanswered(err)
		closes := errors.Is(ctxErr, context.Canceled) || !undecided
		if closes {
			a.invalidateCandidateMaterialEligibility()
		}
		if a.log != nil {
			message := "closed transport refresh kept material eligibility after an undecided failure"
			if closes {
				message = "closed transport refresh invalidated material eligibility"
			}
			a.log.DebugContext(ctx, message, "err_class", closedMemberErrorClass(err), "ctx_class", closedMemberErrorClass(ctxErr), "undecided", undecided)
		}
		return err
	}
}
