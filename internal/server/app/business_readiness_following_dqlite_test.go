//go:build linux && dqlite

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNativeFollowingRenewalUsesRealProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	f, _, _ := newAliasBootstrapMaterialFixture(t, ctx, []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"})
	for i, bundle := range f.bundles {
		if err := replicateRequiredMaterials(ctx, bundle.dataDir, f.stores[i], bundle, "", true, func() {}, enrollmentNativeLog()); err != nil {
			t.Fatal(err)
		}
	}
	bundle, st := f.bundles[0], f.stores[0]
	candidate := &App{cfg: Config{DataDir: bundle.dataDir}, store: st, memberState: memberState{joinedClosed: true}, log: enrollmentNativeLog()}
	phaseStore := func(work context.Context) (*store.Store, error) {
		leader, err := f.databases[0].Leader(work)
		if err != nil {
			return nil, err
		}
		for i, database := range f.databases {
			if database.ID == leader.ID {
				return f.stores[i], nil
			}
		}
		return nil, fmt.Errorf("native phase leader %d is outside fixture", leader.ID)
	}
	for _, mode := range []string{"slow-then-fast", "phase-frozen-before-following-proof"} {
		t.Run(mode, func(t *testing.T) {
			base := time.Now()
			var age atomic.Int64
			var proofs, reads atomic.Int32
			var phase store.MaintenancePhaseState
			entered, release, readDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			gate := newBusinessHandlerGate()
			e := newMaterialEligibility(func() time.Time {
				return base.Add(time.Duration(age.Load()))
			}, func(work context.Context) (bool, error) {
				n := proofs.Add(1)
				if n == 2 {
					close(entered)
					select {
					case <-release:
					case <-work.Done():
						return false, work.Err()
					}
				}
				if n > 3 {
					<-work.Done()
					return false, work.Err()
				}
				if n == 3 && mode == "phase-frozen-before-following-proof" {
					// A cluster upgrade freezes the phase. The real proof's
					// grant is then refused, so readiness is not published.
					leader, err := phaseStore(work)
					if err != nil {
						return false, err
					}
					current, err := leader.ReadMaintenancePhase(work)
					if err != nil {
						return false, err
					}
					if phase, err = leader.TransitionMaintenancePhase(work, current, store.MaintenanceFreezing); err != nil {
						return false, err
					}
				}
				ready, err := candidate.CandidateMemberReady(work)
				if n == 2 && ready && err == nil {
					age.Store(int64(materialEligibilityWindow - 320*time.Millisecond))
				}
				if n == 3 && ready && err == nil {
					age.Store(int64(materialEligibilityWindow - 220*time.Millisecond))
				}
				return ready, err
			}, materialReproofInterval)
			// Use the real gate's synchronous retirement boundary. Default tests
			// separately prove failed-proof/fast-success generation fencing.
			e.setOnInvalidated(func(admissionCloseCause) { gate.setBusinessHandler(nil) })
			if ready, err := e.Reprove(ctx); !ready || err != nil {
				t.Fatalf("initial real candidate proof: %v %v", ready, err)
			}
			age.Store(int64(store.ClusterMembershipGrantTimeout - time.Second))
			work, stopWork := context.WithCancel(ctx)
			runDone := make(chan struct{})
			go func() { defer close(runDone); e.Run(work) }()
			t.Cleanup(func() { stopWork(); <-runDone })
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Leave less than one grant on the initial proof, so readiness must
			// wait for the already-running renewal without repeating SQL.
			age.Store(int64(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second))
			signer, err := auth.NewTokenSigner("native-following-private", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			const hook = "test:private-native-following-SQL"
			if err := st.DB().Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "settings" {
					if reads.Add(1) == 1 && tx.Error == nil {
						close(readDone)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Join the proof before restoring shared state.
				stopWork()
				<-runDone
				if phase.Revision != 0 {
					leader, err := phaseStore(ctx)
					if err == nil {
						_, err = leader.TransitionMaintenancePhase(ctx, phase, store.MaintenanceServing)
					}
					if err != nil {
						t.Error(err)
					}
				}
				_ = st.DB().Callback().Query().Remove(hook)
			})
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(e)
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}, nil))
			request, stopRequest := context.WithCancel(ctx)
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(request))
			}()
			t.Cleanup(func() { stopRequest(); <-done })
			select {
			case <-readDone:
			case <-done:
				t.Fatalf("no successful settings read: status%d body=%s", rec.Code, rec.Body.String())
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-done:
				t.Fatalf("readiness returned before its blocked renewal: status%d", rec.Code)
			case <-time.After(25 * time.Millisecond):
			}
			close(release)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			want := http.StatusServiceUnavailable
			if mode == "slow-then-fast" {
				want = http.StatusOK
			}
			if rec.Code != want || reads.Load() != 1 || proofs.Load() < 3 {
				t.Fatalf("native status%d want%d SQL%d proofs%d", rec.Code, want, reads.Load(), proofs.Load())
			}
			if mode == "phase-frozen-before-following-proof" && (phase.Revision == 0 || phase.Phase != store.MaintenanceFreezing) {
				t.Fatalf("phase rejection lacked a committed freeze: %+v", phase)
			}
			if rec.Code == http.StatusOK && !e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("native readiness lost final five-second headroom")
			}
		})
	}
}
