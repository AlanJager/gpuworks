//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Both responses come from production material handlers with real pinned mTLS
// and confirmed SQL grants. The slower handler has sent all but its final body
// byte; it must not vote and must drain before the proof returns.
func TestCandidatePeerQuorumCancelsAndJoinsSlowAuthenticatedBody(t *testing.T) {
	winner := newCandidateRemoteSetFixture(t, 7)
	wp, wm, _ := candidateRemoteTwoSources(t, winner)
	loser := newCandidateRemoteSetFixture(t, 8)
	lp, lm, _ := candidateRemoteTwoSources(t, loser)
	entered, drained := make(chan struct{}), make(chan struct{})
	loser.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
		defer close(drained)
		actual := httptest.NewRecorder()
		loser.handler.ServeHTTP(actual, r)
		if actual.Code != http.StatusOK {
			t.Errorf("actual authorized handler status=%d", actual.Code)
			return true
		}
		for key, values := range actual.Header() {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(actual.Code)
		_, _ = w.Write(actual.Body.Bytes()[:actual.Body.Len()-1])
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		return true
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runCandidatePeerQuorum(ctx, 2, 1, 2, func(workCtx context.Context, index int) (bool, error) {
		if index == 0 {
			select {
			case <-entered:
			case <-workCtx.Done():
				return false, workCtx.Err()
			}
			err := verifyCandidateRemoteMaterialSets(workCtx, winner.client, winner.server.URL, wp, wm, 7)
			return err == nil, err
		}
		err := verifyCandidateRemoteMaterialSets(workCtx, loser.client, loser.server.URL, lp, lm, 8)
		return err == nil, err
	})
	if err != nil {
		t.Fatalf("complete majority waited for partial authenticated peer: %v (caller=%v)", err, ctx.Err())
	}
	if ctx.Err() != nil {
		t.Fatal("proof succeeded only after its caller deadline")
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("losing authenticated handler not canceled and drained")
	}
	if len(winner.takeRequests()) != 1 || len(loser.takeRequests()) != 1 {
		t.Fatal("fixture did not read both real peer endpoints")
	}
}

// Keep a real body error's context classification visible to the majority
// owner; an integrity mismatch cannot be reclassified merely from ctx.Err.
func TestCandidatePeerQuorumRejectsIntegrityFailureBeforeWinner(t *testing.T) {
	problem := errors.New("observed wrong canonical bytes")
	err := runCandidatePeerQuorum(t.Context(), 2, 1, 2, func(ctx context.Context, index int) (bool, error) {
		if index == 0 {
			return false, problem
		}
		<-ctx.Done()
		return false, ctx.Err()
	})
	if !errors.Is(err, problem) {
		t.Fatalf("observed integrity error lost: %v", err)
	}
}

func TestCandidatePeerQuorumRequiresTwoCompletePeersAndJoins(t *testing.T) {
	entered, drained := make(chan struct{}), make(chan struct{})
	var active, complete atomic.Int32
	err := runCandidatePeerQuorum(t.Context(), 3, 2, 2, func(ctx context.Context, index int) (bool, error) {
		active.Add(1)
		defer active.Add(-1)
		switch index {
		case 0:
			complete.Add(1)
			return true, nil
		case 1:
			select {
			case <-entered:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			complete.Add(1)
			return true, nil
		default:
			close(entered)
			<-ctx.Done()
			close(drained)
			return false, ctx.Err()
		}
	})
	if err != nil || complete.Load() != 2 || active.Load() != 0 {
		t.Fatalf("incomplete majority or undrained checks: complete=%d active=%d err=%v", complete.Load(), active.Load(), err)
	}
	select {
	case <-drained:
	default:
		t.Fatal("returned before losing check drained")
	}
}

func TestCandidatePeerQuorumDoesNotCountPartialOrUnavailableCopies(t *testing.T) {
	var calls atomic.Int32
	err := runCandidatePeerQuorum(t.Context(), 3, 2, 2, func(_ context.Context, index int) (bool, error) { calls.Add(1); return index == 0, nil })
	if err == nil || calls.Load() != 3 {
		t.Fatalf("partial copies formed majority: calls=%d err=%v", calls.Load(), err)
	}
}

func TestCandidatePeerQuorumRejectsConcurrentIntegrityFailureAfterWinner(t *testing.T) {
	entered := make(chan struct{})
	problem := errors.New("observed canonical digest differs")
	var active atomic.Int32
	err := runCandidatePeerQuorum(t.Context(), 2, 1, 2, func(ctx context.Context, index int) (bool, error) {
		active.Add(1)
		defer active.Add(-1)
		if index == 0 {
			<-entered
			return true, nil
		}
		close(entered)
		<-ctx.Done()
		return false, problem
	})
	if !errors.Is(err, problem) || active.Load() != 0 {
		t.Fatalf("winning vote hid concurrent integrity failure: active=%d err=%v", active.Load(), err)
	}
}

func TestCandidatePeerQuorumCallerCancellationAlwaysFails(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	var active atomic.Int32
	err := runCandidatePeerQuorum(ctx, 2, 1, 2, func(workCtx context.Context, index int) (bool, error) {
		active.Add(1)
		defer active.Add(-1)
		if index == 0 {
			<-entered
			cancel()
			return true, nil
		}
		close(entered)
		<-workCtx.Done()
		return false, workCtx.Err()
	})
	if !errors.Is(err, context.Canceled) || active.Load() != 0 {
		t.Fatalf("caller cancellation authorized/lost checks: active=%d err=%v", active.Load(), err)
	}
	for _, expired := range []bool{false, true} {
		var canceled context.Context
		var stop context.CancelFunc
		if expired {
			canceled, stop = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		} else {
			canceled, stop = context.WithCancel(t.Context())
			stop()
		}
		err := runCandidatePeerQuorum(canceled, 2, 1, 2, func(context.Context, int) (bool, error) { t.Error("scheduled a canceled caller"); return true, nil })
		stop()
		if err == nil {
			t.Fatal("canceled caller authorized quorum")
		}
	}
}

func TestCandidatePeerQuorumChildCancellationBeforeMajorityFails(t *testing.T) {
	err := runCandidatePeerQuorum(t.Context(), 2, 1, 2, func(ctx context.Context, index int) (bool, error) {
		if index == 0 {
			return false, context.Canceled
		}
		<-ctx.Done()
		return false, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || t.Context().Err() != nil {
		t.Fatalf("child failure was counted or canceled parent: %v", err)
	}
}

func TestCandidatePeerQuorumRejectsAuthenticatedWrongMember(t *testing.T) {
	peer := newCandidateRemoteSetFixture(t, 7)
	p, m, _ := candidateRemoteTwoSources(t, peer)
	err := runCandidatePeerQuorum(t.Context(), 2, 1, 2, func(ctx context.Context, index int) (bool, error) {
		if index == 0 {
			err := verifyCandidateRemoteMaterialSets(ctx, peer.client, peer.server.URL, p, m, 8)
			return err == nil, err
		}
		<-ctx.Done()
		return false, ctx.Err()
	})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("misidentified authenticated peer authorized quorum: %v", err)
	}
	if len(peer.takeRequests()) != 1 {
		t.Fatal("did not reach real authenticated peer")
	}
}

func TestCandidatePeerQuorumRejectsInvalidThresholds(t *testing.T) {
	for _, values := range [][3]int{{0, 1, 2}, {2, 0, 2}, {2, 3, 2}, {2, 1, 0}} {
		if err := runCandidatePeerQuorum(t.Context(), values[0], values[1], values[2], func(context.Context, int) (bool, error) {
			t.Error("invalid threshold scheduled peers")
			return true, nil
		}); err == nil {
			t.Fatal("invalid quorum bounds accepted")
		}
	}
	if err := runCandidatePeerQuorum(t.Context(), 2, 1, 2, nil); err == nil {
		t.Fatal("missing verifier accepted")
	}
}

func TestCandidatePeerQuorumHedgeStopsAfterCompleteAuthenticatedPeer(t *testing.T) {
	first := newCandidateRemoteSetFixture(t, 7)
	p, m, expected := candidateRemoteTwoSources(t, first)
	second := newCandidateRemoteSetFixture(t, 8)
	sp, sm, _ := candidateRemoteTwoSources(t, second)
	// A fast complete primary must cancel the waiting second callback before
	// that callback issues any authenticated request.
	for attempt := range 24 {
		var calls atomic.Int32
		err := runCandidateHedgedPeerQuorum(t.Context(), 2, 1, 2, materialReproofInterval, func(ctx context.Context, index int) (bool, error) {
			calls.Add(1)
			if index == 0 {
				err := verifyCandidateRemoteMaterialSets(ctx, first.client, first.server.URL, p, m, 7)
				return err == nil, err
			}
			err := verifyCandidateRemoteMaterialSets(ctx, second.client, second.server.URL, sp, sm, 8)
			return err == nil, err
		})
		if err != nil || calls.Load() != 1 {
			t.Fatalf("attempt %d started unnecessary peer: checks=%d err=%v", attempt, calls.Load(), err)
		}
		requests := first.takeRequests()
		if len(requests) != 1 || len(second.takeRequests()) != 0 {
			t.Fatal("majority issued unnecessary authenticated request")
		}
		assertCandidateRemoteSetReads(t, requests, expected)
	}
}

func TestCandidatePeerQuorumHedgeFallsBackOnlyForUnavailablePeer(t *testing.T) {
	for _, shape := range []string{"unavailable", "wrong-member"} {
		t.Run(shape, func(t *testing.T) {
			first := newCandidateRemoteSetFixture(t, 7)
			p, m, _ := candidateRemoteTwoSources(t, first)
			second := newCandidateRemoteSetFixture(t, 8)
			sp, sm, expected := candidateRemoteTwoSources(t, second)
			member := uint64(7)
			if shape == "unavailable" {
				first.server.Close()
			} else {
				member = 9
			}
			var calls atomic.Int32
			started := time.Now()
			err := runCandidateHedgedPeerQuorum(t.Context(), 2, 1, 2, materialReproofInterval, func(ctx context.Context, index int) (bool, error) {
				calls.Add(1)
				var err error
				if index == 0 {
					err = verifyCandidateRemoteMaterialSets(ctx, first.client, first.server.URL, p, m, member)
				} else {
					err = verifyCandidateRemoteMaterialSets(ctx, second.client, second.server.URL, sp, sm, 8)
				}
				if peerUnreachable(err) {
					return false, nil
				}
				return err == nil, err
			})
			if shape == "unavailable" {
				requests := second.takeRequests()
				if err != nil || calls.Load() != 2 || len(requests) != 1 || time.Since(started) >= materialReproofInterval {
					t.Fatalf("unavailable peer prevented complete fallback: checks=%d requests=%d err=%v", calls.Load(), len(requests), err)
				}
				assertCandidateRemoteSetReads(t, requests, expected)
			} else if err == nil || calls.Load() != 1 || len(first.takeRequests()) != 1 || len(second.takeRequests()) != 0 {
				t.Fatalf("integrity error fell back or authorized: checks=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestCandidatePeerQuorumHedgesAuthenticatedPeerWithoutHeaders(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal-entry", true: "late-entry"}[late], func(t *testing.T) {
			first := newCandidateRemoteSetFixture(t, 7)
			p, m, _ := candidateRemoteTwoSources(t, first)
			second := newCandidateRemoteSetFixture(t, 8)
			sp, sm, expected := candidateRemoteTwoSources(t, second)
			entered, drained := make(chan struct{}), make(chan struct{})
			first.beforeServe = func(_ http.ResponseWriter, r *http.Request) bool {
				close(entered)
				defer close(drained)
				<-r.Context().Done()
				return true
			}
			budget, delay := time.Second, 50*time.Millisecond
			if late {
				budget, delay = 200*time.Millisecond, materialReproofInterval
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			err := runCandidateHedgedPeerQuorum(ctx, 2, 1, 2, delay, func(workCtx context.Context, index int) (bool, error) {
				var err error
				if index == 0 {
					err = verifyCandidateRemoteMaterialSets(workCtx, first.client, first.server.URL, p, m, 7)
				} else {
					select {
					case <-entered:
					case <-workCtx.Done():
						return false, workCtx.Err()
					}
					err = verifyCandidateRemoteMaterialSets(workCtx, second.client, second.server.URL, sp, sm, 8)
				}
				if peerUnreachable(err) {
					return false, nil
				}
				return err == nil, err
			})
			if err != nil || ctx.Err() != nil {
				t.Fatalf("authenticated silent primary hid complete fallback: err=%v caller=%v", err, ctx.Err())
			}
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("silent authenticated primary was not canceled and drained")
			}
			requests := second.takeRequests()
			if len(first.takeRequests()) != 1 || len(requests) != 1 {
				t.Fatal("did not read both real authenticated peer endpoints")
			}
			assertCandidateRemoteSetReads(t, requests, expected)
		})
	}
}

func TestCandidatePeerQuorumHedgeRetainsStartedPeerIntegrityError(t *testing.T) {
	first := newCandidateRemoteSetFixture(t, 7)
	p, m, _ := candidateRemoteTwoSources(t, first)
	second := newCandidateRemoteSetFixture(t, 8)
	sp, sm, _ := candidateRemoteTwoSources(t, second)
	observed := make(chan struct{})
	err := runCandidateHedgedPeerQuorum(t.Context(), 2, 1, 2, time.Millisecond, func(ctx context.Context, index int) (bool, error) {
		if index == 0 {
			select {
			case <-observed:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			err := verifyCandidateRemoteMaterialSets(ctx, first.client, first.server.URL, p, m, 7)
			return err == nil, err
		}
		err := verifyCandidateRemoteMaterialSets(ctx, second.client, second.server.URL, sp, sm, 9)
		close(observed)
		<-ctx.Done()
		return false, err
	})
	if err == nil || errors.Is(err, context.Canceled) || len(first.takeRequests()) != 1 || len(second.takeRequests()) != 1 {
		t.Fatalf("majority hid an already observed authenticated identity error: %v", err)
	}
}

func TestCandidatePeerQuorumHedgeCancellationDoesNotAuthorize(t *testing.T) {
	for _, caller := range []bool{false, true} {
		t.Run(map[bool]string{false: "child-only", true: "caller"}[caller], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var active, calls atomic.Int32
			err := runCandidateHedgedPeerQuorum(ctx, 2, 1, 2, time.Hour, func(context.Context, int) (bool, error) {
				active.Add(1)
				defer active.Add(-1)
				calls.Add(1)
				if caller {
					cancel()
					return true, nil
				}
				return false, context.Canceled
			})
			if !errors.Is(err, context.Canceled) || calls.Load() != 1 || active.Load() != 0 || (!caller && ctx.Err() != nil) {
				t.Fatalf("cancellation authorized, started backup, or left work: calls=%d active=%d err=%v", calls.Load(), active.Load(), err)
			}
		})
	}
}

func TestCandidatePeerQuorumHedgeRetainsMultiPeerThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var complete atomic.Int32
	err := runCandidateHedgedPeerQuorum(ctx, 3, 2, 2, time.Hour, func(context.Context, int) (bool, error) {
		complete.Add(1)
		return true, nil
	})
	if err != nil || complete.Load() < 2 || ctx.Err() != nil {
		t.Fatalf("larger majority was reduced or unnecessarily hedged: complete=%d err=%v", complete.Load(), err)
	}
}
