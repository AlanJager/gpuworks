//go:build linux && dqlite

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBusinessHandlerGateClosesWhenMaterialProofExpires(t *testing.T) {
	now := time.Now().UTC()
	eligibility := newMaterialEligibility(func() time.Time { return now }, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	majority := true
	gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
		return guardBusinessRequests(next, store.ClusterIdentity{}, 1, "local-key", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
			if !majority {
				return 0, nil, context.DeadlineExceeded
			}
			return 1, []store.ClusterTransportMember{{ID: 1, KeyFingerprint: "local-key"}, {ID: 2, KeyFingerprint: "peer-key"}}, nil
		})
	})
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	request := func(path string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("GET %s: status=%d, want %d", path, rec.Code, want)
		}
	}
	request("/api/v1/nodes", http.StatusServiceUnavailable)
	gate.setMaterialEligibility(eligibility)
	request("/api/v1/nodes", http.StatusServiceUnavailable)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("material proof: ready=%t err=%v", ready, err)
	}
	request("/api/v1/nodes", http.StatusAccepted)
	majority = false
	request("/api/v1/nodes", http.StatusServiceUnavailable)
	majority = true
	now = now.Add(materialEligibilityWindow)
	request("/api/v1/nodes", http.StatusServiceUnavailable)
	request("/healthz", http.StatusOK)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("renewed material proof: ready=%t err=%v", ready, err)
	}
	request("/api/v1/nodes", http.StatusAccepted)
	eligibility.Invalidate()
	request("/api/v1/nodes", http.StatusServiceUnavailable)
}

func TestBusinessHandlerGateRechecksAdmissionAfterMajorityWait(t *testing.T) {
	for _, change := range []string{"expired", "invalidated", "withdrawn"} {
		t.Run(change, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().UnixNano())
			eligibility := newMaterialEligibility(func() time.Time { return time.Unix(0, clock.Load()) },
				func(context.Context) (bool, error) { return true, nil }, time.Hour)
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("initial material proof: ready=%t err=%v", ready, err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(eligibility)
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 1, "local-key",
					func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
						close(entered)
						<-release
						return 1, []store.ClusterTransportMember{{ID: 1, KeyFingerprint: "local-key"}, {ID: 2, KeyFingerprint: "peer-key"}}, nil
					})
			})
			called := make(chan struct{}, 1)
			gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called <- struct{}{}
				w.WriteHeader(http.StatusAccepted)
			}))
			finished := make(chan int, 1)
			go func() {
				recorder := httptest.NewRecorder()
				gate.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
				finished <- recorder.Code
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not reach majority check")
			}
			switch change {
			case "expired":
				clock.Add(materialEligibilityWindow.Nanoseconds())
			case "invalidated":
				eligibility.Invalidate()
			case "withdrawn":
				gate.setBusinessHandler(nil)
			}
			close(release)
			select {
			case status := <-finished:
				if status != http.StatusServiceUnavailable {
					t.Fatalf("revoked admission entered business: status=%d", status)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request did not finish after majority check")
			}
			select {
			case <-called:
				t.Fatal("business handler ran after admission was revoked")
			default:
			}
		})
	}
}

func TestClosedAppMaterialWorkerGatesInstalledBusinessHandler(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, businessHTTP: gate, materialEligibility: eligibility}}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := a.startClosedMaterialEligibility(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := a.stopClosedMaterialEligibility(context.Background()); err != nil {
			t.Error(err)
		}
	})
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	deadline := time.Now().Add(2 * time.Second)
	for !eligibility.Eligible() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !eligibility.Eligible() {
		t.Fatal("closed App worker did not prove material")
	}
	request := func(want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
		if rec.Code != want {
			t.Fatalf("business gate status=%d, want %d", rec.Code, want)
		}
	}
	request(http.StatusAccepted)
	eligibility.Invalidate()
	request(http.StatusServiceUnavailable)
}

func TestBusinessHandlerGateSwitchesWhileServing(t *testing.T) {
	gate := newBusinessHandlerGate()
	server := httptest.NewServer(gate)
	defer server.Close()
	client := server.Client()

	probe := func(path string, want int) {
		t.Helper()
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET %s: status=%d body=%q, want %d", path, response.StatusCode, body, want)
		}
	}

	probe("/api/v1/nodes", http.StatusServiceUnavailable)
	probe("/healthz", http.StatusOK)

	const callers = 8
	stop := make(chan struct{})
	var workers sync.WaitGroup
	var failures sync.Once
	failed := make(chan string, 1)
	for range callers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				response, err := client.Get(server.URL + "/api/v1/nodes")
				if err != nil {
					failures.Do(func() { failed <- err.Error() })
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusServiceUnavailable && response.StatusCode != http.StatusAccepted {
					failures.Do(func() { failed <- response.Status })
					return
				}
			}
		}()
	}

	business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for range 12 {
		gate.setBusinessHandler(business)
		probe("/api/v1/nodes", http.StatusAccepted)
		probe("/healthz", http.StatusOK)
		gate.setBusinessHandler(nil)
		probe("/api/v1/nodes", http.StatusServiceUnavailable)
		probe("/healthz", http.StatusOK)
	}
	close(stop)
	workers.Wait()
	select {
	case message := <-failed:
		t.Fatal(message)
	default:
	}
}

func TestBusinessHandlerGateDrainsInFlightHandlerAfterClosing(t *testing.T) {
	gate := newBusinessHandlerGate()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	server := httptest.NewServer(gate)
	defer server.Close()
	client := server.Client()

	finished := make(chan int, 1)
	go func() {
		response, err := client.Get(server.URL + "/api/v1/nodes")
		if err != nil {
			finished <- 0
			return
		}
		_ = response.Body.Close()
		finished <- response.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("business request did not reach its handler")
	}
	gate.setBusinessHandler(nil)
	response, err := client.Get(server.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new request after close: status=%d", response.StatusCode)
	}
	releaseOnce.Do(func() { close(release) })
	if status := <-finished; status != http.StatusAccepted {
		t.Fatalf("in-flight request: status=%d", status)
	}
}

func TestBusinessHandlerGateWithdrawTimeoutAndReplacement(t *testing.T) {
	gate := newBusinessHandlerGate()
	oldEntered := make(chan struct{})
	oldRelease := make(chan struct{})
	newEntered := make(chan struct{})
	newRelease := make(chan struct{})
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(oldEntered)
		<-oldRelease
		w.WriteHeader(http.StatusAccepted)
	}))
	server := httptest.NewServer(gate)
	defer server.Close()
	var releaseOld, releaseNew sync.Once
	defer releaseOld.Do(func() { close(oldRelease) })
	defer releaseNew.Do(func() { close(newRelease) })
	request := func() <-chan int {
		finished := make(chan int, 1)
		go func() {
			response, err := server.Client().Get(server.URL + "/api/v1/nodes")
			if err != nil {
				finished <- 0
				return
			}
			_ = response.Body.Close()
			finished <- response.StatusCode
		}()
		return finished
	}
	oldFinished := request()
	select {
	case <-oldEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("old request did not enter")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	drain, err := gate.withdrawBusinessHandler(ctx)
	if err != context.DeadlineExceeded || drain == nil {
		t.Fatalf("withdrawal: drain=%v err=%v, want retained drain and deadline", drain, err)
	}
	closed := request()
	if status := <-closed; status != http.StatusServiceUnavailable {
		t.Fatalf("request after withdrawal: status=%d, want 503", status)
	}
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(newEntered)
		<-newRelease
		w.WriteHeader(http.StatusAccepted)
	}))
	newFinished := request()
	select {
	case <-newEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement request did not enter")
	}
	releaseOld.Do(func() { close(oldRelease) })
	drainCtx, stopDrain := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopDrain()
	if err := drain.wait(drainCtx); err != nil {
		t.Fatalf("old generation did not drain: %v", err)
	}
	if status := <-oldFinished; status != http.StatusAccepted {
		t.Fatalf("old request: status=%d, want 202", status)
	}
	select {
	case status := <-newFinished:
		t.Fatalf("replacement request finished before release: status=%d", status)
	default:
	}
	releaseNew.Do(func() { close(newRelease) })
	if status := <-newFinished; status != http.StatusAccepted {
		t.Fatalf("replacement request: status=%d, want 202", status)
	}
}
