//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialEligibilityClosedSlowFailureRestsWithReadinessGuard(t *testing.T) {
	const interval = 200 * time.Millisecond
	entered, release, recoverProof := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finished, next := make(chan time.Time, 1), make(chan time.Time, 1)
	var calls atomic.Int32
	e := newMaterialEligibility(nil, func(ctx context.Context) (bool, error) {
		switch calls.Add(1) {
		case 1:
			close(entered)
			select {
			case <-release:
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		case 2:
			next <- time.Now()
			select {
			case <-recoverProof:
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		default:
			<-ctx.Done()
			return false, ctx.Err()
		}
	}, interval)
	var closed sync.Once
	e.setOnInvalidated(func(admissionCloseCause) { closed.Do(func() { finished <- time.Now() }) })
	gate, reads := closedRetryReadiness(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("closed retry worker did not join")
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first proof was not immediate")
	}
	<-time.After(2 * interval)
	close(release)
	completed := <-finished
	e.mu.Lock()
	deadline, epoch := e.deadline, e.epoch
	e.mu.Unlock()
	if !deadline.IsZero() || epoch != 0 || e.Eligible() {
		t.Fatal("false,nil did not close original proof", deadline, epoch)
	}
	assertClosedRetryHTTP(t, gate, http.StatusServiceUnavailable)
	if reads.Load() != 0 {
		t.Fatal("closed readiness reached SQL", reads.Load())
	}
	select {
	case started := <-next:
		if gap := started.Sub(completed); gap < interval*3/4 {
			t.Fatalf("closed slow false,nil consumed stale tick: gap=%s interval=%s", gap, interval)
		}
	case <-time.After(3 * interval):
		t.Fatal("closed member never retried")
	}
	assertClosedRetryHTTP(t, gate, http.StatusServiceUnavailable)
	close(recoverProof)
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for !e.Eligible() {
		select {
		case <-time.After(time.Millisecond):
		case <-timeout.C:
			t.Fatal("complete proof did not restore eligibility")
		}
	}
	assertClosedRetryHTTP(t, gate, http.StatusOK)
	if reads.Load() != 1 {
		t.Fatal("readiness did not make its one SQL read", reads.Load())
	}
}

func closedRetryReadiness(t *testing.T, e *materialEligibility) (*businessHandlerGate, *atomic.Int32) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "closed-retry.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("closed-retry-fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reads := new(atomic.Int32)
	if err := st.DB().Callback().Query().After("gorm:query").Register("test:closed-retry-readyz", func(tx *gorm.DB) {
		if tx.Error == nil && tx.Statement.Table == "settings" {
			reads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(e)
	gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}, nil))
	return gate, reads
}

func assertClosedRetryHTTP(t *testing.T, gate *businessHandlerGate, status int) {
	t.Helper()
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if response.Code != status {
		t.Fatalf("actual readiness HTTP=%d want%d body=%q", response.Code, status, response.Body.String())
	}
	if status == http.StatusOK && response.Body.String() != "{\"status\":\"ready\"}\n" {
		t.Fatalf("unexpected readiness response %q", response.Body.String())
	}
}

func TestMaterialEligibilityClosedFailureClassesRest(t *testing.T) {
	const interval = 200 * time.Millisecond
	for _, mode := range []string{"independent-error", "expired-success", "expired-deadline", "proof-context-deadline"} {
		t.Run(mode, func(t *testing.T) {
			clock := &materialTestClock{now: time.Now()}
			entered, release := make(chan struct{}), make(chan struct{})
			completed, next := make(chan time.Time, 1), make(chan time.Time, 1)
			var calls atomic.Int32
			e := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
				if calls.Add(1) != 1 {
					next <- time.Now()
					<-ctx.Done()
					return false, ctx.Err()
				}
				close(entered)
				if mode == "proof-context-deadline" {
					<-ctx.Done()
					return false, ctx.Err()
				}
				select {
				case <-release:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				if mode == "independent-error" {
					return false, errors.New("independent peer material failure")
				}
				clock.Advance(materialEligibilityWindow + time.Nanosecond)
				if mode == "expired-success" {
					return true, nil
				}
				return false, context.DeadlineExceeded
			}, interval)
			var once sync.Once
			e.setOnInvalidated(func(admissionCloseCause) { once.Do(func() { completed <- time.Now() }) })
			gate, reads := closedRetryReadiness(t, e)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); e.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("closed failure worker did not join")
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first proof did not start")
			}
			<-time.After(2 * interval)
			close(release)
			var finished time.Time
			select {
			case finished = <-completed:
			case <-time.After(materialEligibilityWindow + time.Second):
				t.Fatal("original proof deadline did not close member")
			}
			e.mu.Lock()
			deadline, epoch := e.deadline, e.epoch
			e.mu.Unlock()
			if !deadline.IsZero() || e.Eligible() {
				t.Fatal("failed or expired proof retained authority", mode, deadline)
			}
			assertClosedRetryHTTP(t, gate, http.StatusServiceUnavailable)
			if reads.Load() != 0 {
				t.Fatal("closed proof reached business SQL")
			}
			select {
			case started := <-next:
				if started.Sub(finished) < interval*3/4 {
					t.Fatal("closed failure consumed pending tick", mode, started.Sub(finished))
				}
			case <-time.After(3 * interval):
				t.Fatal("closed failure never retried", mode)
			}
			e.mu.Lock()
			unchanged := e.deadline.IsZero() && e.epoch == epoch
			e.mu.Unlock()
			if !unchanged {
				t.Fatal("scheduling changed closed deadline or epoch")
			}
			assertClosedRetryHTTP(t, gate, http.StatusServiceUnavailable)
		})
	}
}

func TestMaterialEligibilityClosedRestCancellationJoinsWithoutExtraProof(t *testing.T) {
	const interval = 400 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	completed := make(chan struct{}, 1)
	var calls atomic.Int32
	e := newMaterialEligibility(nil, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		<-ctx.Done()
		return false, ctx.Err()
	}, interval)
	var once sync.Once
	e.setOnInvalidated(func(admissionCloseCause) { once.Do(func() { completed <- struct{}{} }) })
	gate, reads := closedRetryReadiness(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial proof did not start")
	}
	<-time.After(2 * interval)
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("negative proof did not complete")
	}
	assertClosedRetryHTTP(t, gate, http.StatusServiceUnavailable)
	cancel()
	select {
	case <-done:
	case <-time.After(interval / 2):
		t.Fatal("closed rest ignored cancellation")
	}
	if calls.Load() != 1 || reads.Load() != 0 {
		t.Fatal("closed cancellation started extra proof or SQL", calls.Load(), reads.Load())
	}
}

func TestMaterialEligibilityLiveEarlierProofRetainsFastDeadlineRenewal(t *testing.T) {
	const interval = 400 * time.Millisecond
	for _, thin := range []bool{false, true} {
		t.Run(map[bool]string{false: "ample-headroom", true: "eligible-but-not-fresh-five"}[thin], func(t *testing.T) {
			clock := &materialTestClock{now: time.Now()}
			entered, release := make(chan struct{}), make(chan struct{})
			finished, next := make(chan time.Time, 1), make(chan time.Time, 1)
			active := make(chan context.Context, 1)
			var calls atomic.Int32
			e := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
				switch calls.Add(1) {
				case 1:
					return true, nil
				case 2:
					close(entered)
					select {
					case <-release:
						finished <- time.Now()
						return false, fmt.Errorf("membership grant: %w", context.DeadlineExceeded)
					case <-ctx.Done():
						return false, ctx.Err()
					}
				default:
					next <- time.Now()
					active <- ctx
					<-ctx.Done()
					return false, ctx.Err()
				}
			}, interval)
			if ready, err := e.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			e.mu.Lock()
			deadline, epoch := e.deadline, e.epoch
			e.mu.Unlock()
			if thin {
				clock.Advance(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second)
			} else {
				clock.Advance(time.Second)
			}
			gate, reads := closedRetryReadiness(t, e)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); e.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("live renewal worker did not join")
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("startup renewal was not immediate")
			}
			<-time.After(2 * interval)
			close(release)
			returned := <-finished
			select {
			case started := <-next:
				if started.Sub(returned) >= interval*3/4 {
					t.Fatal("eligible prior proof lost pending-tick renewal", started.Sub(returned))
				}
			case <-time.After(interval * 3 / 4):
				t.Fatal("eligible prior proof slept closed interval")
			}
			proofCtx := <-active
			if proofCtx.Err() != nil || !e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout) == thin {
				t.Fatal("prior eligibility/headroom or live proof changed")
			}
			if thin {
				requestCtx, stopRequest := context.WithTimeout(t.Context(), 100*time.Millisecond)
				response := httptest.NewRecorder()
				gate.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(requestCtx))
				stopRequest()
				if response.Code != http.StatusServiceUnavailable {
					t.Fatal("thin prior proof advertised readiness", response.Code)
				}
				ordinary := httptest.NewRecorder()
				gate.ServeHTTP(ordinary, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
				if ordinary.Code != http.StatusUnauthorized {
					t.Fatal("ordinary eligibility was withdrawn", ordinary.Code)
				}
			} else {
				assertClosedRetryHTTP(t, gate, http.StatusOK)
			}
			if reads.Load() != 1 {
				t.Fatal("request SQL chain changed", reads.Load())
			}
			e.mu.Lock()
			unchanged := e.deadline == deadline && e.epoch == epoch
			e.mu.Unlock()
			if !unchanged || !e.Eligible() || proofCtx.Err() != nil || calls.Load() != 3 {
				t.Fatal("deadline-only renewal changed old authority or proof context")
			}
		})
	}
}
