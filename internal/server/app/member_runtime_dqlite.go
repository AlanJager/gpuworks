//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/restorehold"
)

// memberRuntime is the haRuntime of a server on a replicated store, in one
// of three roles:
//
//   - a closed member (its App has joinedClosed set): a joining or
//     established member. The App has no business; Run serves the closed
//     listener, proves the member's materials against the cluster and opens
//     business generations while the proof holds.
//   - a generation (borrowed): the business App of one such generation. It
//     borrows the closed member's store, listeners and enrollment endpoint.
//   - a single member, whose business is open on its own store.
type memberRuntime struct {
	// member is what New established before any store; a closed member
	// assembles every generation from it.
	member closedMember
	// bundle is the local enrollment material; nil on a single member that
	// was never initialized as a cluster.
	bundle *enrollmentBundle
	// owner is the closed member's enrollment endpoint a generation prepares
	// its routes for; holdScheduling keeps a restored member from scheduling
	// until the old member is confirmed isolated.
	owner          *enrollmentServer
	borrowed       bool
	holdScheduling bool
	// business is the App whose requests this runtime admits.
	business *App
	// forward hands business requests to the member that leads, and serves
	// what the other members hand over (member_forward.go).
	forward *memberForward
}

// newJoiningMemberRuntime is the runtime of a member with a join marker.
func newJoiningMemberRuntime(member closedMember) (*memberRuntime, error) {
	return newClosedMemberRuntime(member)
}

// newEstablishedMemberRuntime is the runtime of a member of an established
// multi-member cluster.
func newEstablishedMemberRuntime(member closedMember) (*memberRuntime, error) {
	return newClosedMemberRuntime(member)
}

func newClosedMemberRuntime(member closedMember) (*memberRuntime, error) {
	bundle, err := readEnrollmentBundle(member.cfg.DataDir, false)
	if err != nil {
		return nil, err
	}
	return &memberRuntime{member: member, bundle: bundle}, nil
}

// newSingleMemberRuntime is the runtime of a single member whose business is
// open on st.
func newSingleMemberRuntime(cfg Config, bundle *enrollmentBundle) (*memberRuntime, error) {
	hold, err := restorehold.SchedulingIsolationRequired(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &memberRuntime{bundle: bundle, holdScheduling: hold}, nil
}

func (r *memberRuntime) generation() bool { return r.borrowed }

func (r *memberRuntime) assemble(a *App, signer *auth.TokenSigner, catalog *packages.Store) error {
	r.business = a
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := verifyEnrollmentState(ctx, a.store, r.bundle)
	cancel()
	if err != nil {
		return err
	}
	if r.borrowed {
		// The generation serves stored deployments as they are: another
		// member may be launching from them while this one opens.
		if err := requireNormalizedLaunchConfigs(a.store); err != nil {
			return err
		}
	}
	enrollment, err := prepareEnrollmentServerWithOwner(r.bundle, a.store, signer, a.log, catalog, r.owner)
	if err != nil {
		return err
	}
	a.enrollment = enrollment
	if r.holdScheduling {
		state := BeginDisasterRecovery()
		a.disaster = &state
	}
	return nil
}

func (r *memberRuntime) runClosed(ctx context.Context, a *App) (bool, error) {
	if err := a.prepareEntry(); err != nil {
		return true, err
	}
	if !a.joinedClosed {
		return false, nil
	}
	return true, a.runClosedJoined(ctx)
}

func (r *memberRuntime) handler(next http.Handler) http.Handler {
	if a := r.business; a != nil && a.store != nil && a.hub != nil {
		r.forward = newMemberForward(a.log, a.store.ObservedActionLeader, a.hub.SessionCount, a.hub.DropSessions)
		// Admission comes first on either end: a member whose business is
		// not ready neither serves nor forwards.
		r.forward.local = memberServiceHandler(next, a)
		r.forward.agents = newMemberAgentRelay(r.forward, a.hub)
		r.forward.answers = a.memberAnswerer
		return memberServiceHandler(r.forward.wrap(next), a)
	}
	return memberServiceHandler(next, r.business)
}

// start runs what a single member keeps beside business. A generation is
// never run: its closed member owns these loops.
func (r *memberRuntime) start(ctx context.Context, a *App, failed chan<- error) (func(), error) {
	stopSessions, err := a.routeClusterMemberSessions(ctx, a.memberSessions())
	if err != nil {
		return nil, err
	}
	closeSessions := func() {
		if stopSessions == nil {
			return
		}
		if err := stopSessions(); err != nil {
			a.log.Warn("member session route did not close cleanly", "err", err)
		}
	}
	if a.enrollment != nil {
		if err := a.enrollment.Start(ctx); err != nil {
			closeSessions()
			return nil, err
		}
	}
	var control sync.WaitGroup
	if a.store.TransportAuthorizationEnabled() {
		control.Go(func() {
			runTransportAuthorization(ctx, a.store.RefreshTransportAuthorization, a.log, time.Second, 5*time.Second)
		})
	}
	if a.enrollment != nil {
		// The enrollment owner tracks every material iteration and bounds its
		// shutdown. Waiting here could block forever in a filesystem sync.
		go a.runMaterialReplication(ctx, time.Minute)
		go func() {
			err := a.enrollment.Wait()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("enrollment HTTPS server stopped unexpectedly")
			}
			select {
			case failed <- err:
			default:
			}
		}()
	}
	return func() {
		control.Wait()
		closeSessions()
		a.stopEnrollment()
	}, nil
}

func (r *memberRuntime) runController(ctx context.Context, a *App, run func(context.Context)) {
	a.runLeaderController(ctx, run)
}

// beforeClose keeps a member's store and runtime lease until every in-flight
// material proof and request has left it.
func (r *memberRuntime) beforeClose(a *App) error {
	if r.borrowed {
		return nil
	}
	return a.withdrawMember(enrollmentShutdownTimeout)
}

func (r *memberRuntime) afterClose(a *App) error {
	if a.joinedRaft == nil {
		return nil
	}
	return a.joinedRaft.Close()
}

// withdrawMember stops the member's business generation, material requests
// and material proof, each within timeout. A failure leaves all three with
// the App for a later Close.
func (a *App) withdrawMember(timeout time.Duration) error {
	businessCtx, businessCancel := context.WithTimeout(context.Background(), timeout)
	err := a.stopClosedBusiness(businessCtx)
	businessCancel()
	if err != nil {
		return err
	}
	if a.enrollment != nil && a.enrollment.materialRequests != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := a.enrollment.materialRequests.withdraw(ctx); err != nil {
			// A forced listener close does not wait for a handler that ignores
			// cancellation. Keep Store and its runtime lease for a later retry.
			return err
		}
	}
	a.materialEligibilityMu.Lock()
	a.materialEligibilityClosed = true
	a.materialEligibilityMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// A proof stuck in external I/O may outlive cancellation. Its Store and
	// runtime lease stay owned by this App until a later Close drains it.
	return a.stopClosedMaterialEligibility(ctx)
}

// assembleBusinessGeneration builds one business generation on the closed
// member's store, from the enrollment material its current proof read.
func (a *App) assembleBusinessGeneration(bundle *enrollmentBundle, holdScheduling bool) (*App, error) {
	closed, ok := a.ha.(*memberRuntime)
	if !ok || !a.joinedClosed {
		return nil, errors.New("business generation requires a closed member")
	}
	member := closed.member
	if err := checkStoredPluginState(a.store); err != nil {
		return nil, err
	}
	// The cluster's key, never a new one: a member that lost master.key must
	// fail here rather than seal credentials no other member can open.
	master, err := enrollmentAppSecret(member.cfg, bundle, member.policy)
	if err != nil {
		return nil, err
	}
	generation := &memberRuntime{bundle: bundle, owner: a.enrollment, borrowed: true, holdScheduling: holdScheduling}
	staged, err := assembleBusiness(member.cfg, a.log, a.bus, a.store, master, generation)
	if err != nil {
		// A failed assembly has closed what it opened; the member's store
		// must not keep the credential key of a generation that never ran.
		a.store.SetSecretbox(nil)
		a.store.ReleaseNodeAdmission()
		return nil, err
	}
	return staged, nil
}

// discardBusinessGeneration releases a generation that was assembled but
// never activated, and takes its credential key back off the member's store.
func (a *App) discardBusinessGeneration(staged *App) error {
	err := staged.Close()
	a.store.SetSecretbox(nil)
	a.store.ReleaseNodeAdmission()
	return err
}
