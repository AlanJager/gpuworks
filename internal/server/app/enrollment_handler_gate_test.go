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
