//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialReadinessFailureKeepsLostLeaderSignal(t *testing.T) {
	ready, err := materialReadinessFailure(t.Context(), driver.ErrBadConn)
	if ready || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost leader became a definitive proof: ready=%t err=%v", ready, err)
	}
	ready, err = materialReadinessFailure(t.Context(), errors.New("material manifest pointer differs from live source"))
	if ready || err != nil {
		t.Fatalf("changed manifest stayed retryable: ready=%t err=%v", ready, err)
	}
	ready, err = materialReadinessFailure(t.Context(), errCandidateMaterialNoMajority)
	if ready || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing peer votes became a definitive proof: ready=%t err=%v", ready, err)
	}
	ready, err = materialReadinessFailure(t.Context(), fmt.Errorf("read remote material batch framing: %w", syscall.ECONNRESET))
	if ready || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reset peer became a definitive proof: ready=%t err=%v", ready, err)
	}
	for name, cause := range map[string]error{
		"no leader":               fmt.Errorf("read manifest: %w", dqlitedriver.ErrNoAvailableLeader),
		"result cut by a failure": fmt.Errorf("read manifest: %w", dqlitedriver.Error{Code: 3, Message: "key"}),
		"leader not read in time": fmt.Errorf("grant: %w", store.ErrClusterMembershipObservationTimeout),
	} {
		ready, err = materialReadinessFailure(t.Context(), cause)
		if ready || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s became a definitive proof: ready=%t err=%v", name, ready, err)
		}
	}
	voted, voteErr := materialPeerVote(errRemoteMaterialBatchUnavailable)
	if voted || voteErr != nil {
		t.Fatalf("503 canceled the other peer: voted=%t err=%v", voted, voteErr)
	}
	voted, voteErr = materialPeerVote(fmt.Errorf("read remote material batch framing: %w", syscall.ECONNRESET))
	if voted || voteErr != nil {
		t.Fatalf("reset canceled the other peer: voted=%t err=%v", voted, voteErr)
	}
	voted, voteErr = materialPeerVote(errors.New("remote material batch response metadata differs"))
	if voted || voteErr == nil {
		t.Fatalf("metadata mismatch stayed a vote: voted=%t err=%v", voted, voteErr)
	}
	previous := []store.ClusterTransportMember{{ID: 1, KeyFingerprint: "a"}, {ID: 2, KeyFingerprint: "b"}, {ID: 3, KeyFingerprint: "c"}}
	shrunk := []store.ClusterTransportMember{{ID: 1, KeyFingerprint: "a"}, {ID: 2, KeyFingerprint: "b"}}
	interrupted, interruptErr := materialMembershipInterrupted(t.Context(), nil, previous, shrunk)
	if !interrupted || !errors.Is(interruptErr, context.DeadlineExceeded) {
		t.Fatalf("lost member closed the proof: interrupted=%t err=%v", interrupted, interruptErr)
	}
	replaced := []store.ClusterTransportMember{{ID: 1, KeyFingerprint: "a"}, {ID: 2, KeyFingerprint: "b"}, {ID: 9, KeyFingerprint: "new"}}
	interrupted, interruptErr = materialMembershipInterrupted(t.Context(), nil, previous, replaced)
	if interrupted || interruptErr != nil {
		t.Fatalf("new member stayed open: interrupted=%t err=%v", interrupted, interruptErr)
	}
}

func TestLostLeaderSignalKeepsOrdinaryAdmission(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	var calls atomic.Int32
	var invalidated atomic.Int32
	eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		return materialReadinessFailure(ctx, driver.ErrBadConn)
	}, time.Second)
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	eligibility.setOnInvalidated(func() {
		invalidated.Add(1)
		gate.setBusinessHandler(nil)
	})
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	settings := func() (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/test.ha.accept", nil)
		gate.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := settings(); code != http.StatusNoContent || body != "" {
		t.Fatalf("open member refused settings: %d %q", code, body)
	}
	// One nanosecond past the grant budget, still inside the original window.
	clock.Advance(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Nanosecond)
	if eligibility.EligibleFor(store.ClusterMembershipGrantTimeout) || !eligibility.Eligible() {
		t.Fatal("fresh proof advertised grant headroom it did not have")
	}
	ready, err := eligibility.Reprove(t.Context())
	if ready || !errors.Is(err, context.DeadlineExceeded) || !eligibility.Eligible() || invalidated.Load() != 0 {
		t.Fatalf("lost leader withdrew an unexpired proof: ready=%t err=%v eligible=%t invalidated=%d", ready, err, eligibility.Eligible(), invalidated.Load())
	}
	if eligibility.EligibleFor(store.ClusterMembershipGrantTimeout) {
		t.Fatal("lost leader extended grant headroom")
	}
	if code, body := settings(); code != http.StatusNoContent || body != "" {
		t.Fatalf("ordinary settings closed after lost leader: %d %q", code, body)
	}
	clock.Advance(store.ClusterMembershipGrantTimeout)
	if eligibility.Eligible() {
		t.Fatal("kept proof outlived its original window")
	}
	if code, body := settings(); code != http.StatusServiceUnavailable || body != "member business service is not ready\n" {
		t.Fatalf("expired proof still served settings: %d %q", code, body)
	}
}
