//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnrollmentHandlerGateWithdrawDrainsPreparedRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	gate := newEnrollmentHandlerGate(http.NotFoundHandler())
	gate.install(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	finished := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		gate.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/business", nil))
		finished <- response.Code
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prepared handler did not accept request")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	drain, err := gate.withdrawAndWait(ctx)
	if err != context.Canceled || drain == nil {
		t.Fatalf("withdrawal must retain timed-out drain: drain=%v error=%v", drain, err)
	}
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/business", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("new request reached prepared handler after withdrawal: %d", response.Code)
	}
	close(release)
	select {
	case status := <-finished:
		if status != http.StatusAccepted {
			t.Fatalf("accepted request failed to finish: %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted request did not finish")
	}
	if err := drain.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// The quorum observation routes belong to the listener itself: installing
// prepared routes neither hides them nor makes the prepared drain wait for
// them.
func TestEnrollmentHandlerGateKeepsQuorumObservationWithTheListener(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_cluster/quorum-facts" {
			close(entered)
			<-release
		}
		w.Header().Set("X-Served-By", "listener")
		w.WriteHeader(http.StatusNoContent)
	})
	gate := newEnrollmentHandlerGate(base)
	serve := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		gate.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		return response
	}
	for _, path := range []string{"/_cluster/quorum-facts-other", "/business"} {
		if got := serve(path); got.Header().Get("X-Served-By") != "listener" {
			t.Fatalf("%s without prepared routes was not served by the listener", path)
		}
	}
	// As the real prepared routes do, this one answers both paths with 404.
	gate.install(http.NotFoundHandler())
	if got := serve("/_cluster/quorum-discovery"); got.Code != http.StatusNoContent || got.Header().Get("X-Served-By") != "listener" {
		t.Fatalf("quorum discovery with prepared routes: %d", got.Code)
	}
	for _, path := range []string{"/business", "/_cluster/quorum-facts/", "/_cluster/quorum-facts-other", "/_cluster"} {
		if got := serve(path); got.Code != http.StatusNotFound {
			t.Fatalf("%s bypassed the prepared routes: %d", path, got.Code)
		}
	}
	held := make(chan int, 1)
	go func() { held <- serve("/_cluster/quorum-facts").Code }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("quorum facts with prepared routes did not reach the listener")
	}
	// The held request is the listener's, so the prepared routes drain at once.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if drain, err := gate.withdrawAndWait(ctx); err != nil || drain == nil {
		t.Fatalf("prepared drain waited for a listener request: drain=%v err=%v", drain, err)
	}
	close(release)
	select {
	case code := <-held:
		if code != http.StatusNoContent {
			t.Fatalf("held quorum facts: %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("held quorum facts did not finish")
	}
}
