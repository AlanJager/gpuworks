//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type waitingRaftSession struct{ closes atomic.Int32 }

func (s *waitingRaftSession) Close() error {
	s.closes.Add(1)
	return nil
}

func TestWaitingJoinedListenerKeepsBusinessClosedAndReleasesSession(t *testing.T) {
	session := &waitingRaftSession{}
	gate := newBusinessHandlerGate()
	a := &App{cfg: Config{Addr: "127.0.0.1:0"}, http: &http.Server{Handler: gate}, ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, legacyClosed: true, joinedRaft: session, businessHTTP: gate}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("closed listener failed to stop")
			}
		})
	}
	t.Cleanup(func() { stop(); _ = a.Close() })
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.Addr(); addr != "127.0.0.1:0" {
			response, err := client.Get("http://" + addr + "/healthz")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, path := range []string{"/healthz", "/api/v1/healthz", "/v1/models"} {
		response, err := client.Get("http://" + a.Addr() + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := http.StatusServiceUnavailable
		if path == "/healthz" {
			want = http.StatusOK
		}
		if response.StatusCode != want {
			t.Fatalf("%s: status %d, want %d", path, response.StatusCode, want)
		}
	}
	stop()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if got := session.closes.Load(); got != 1 {
		t.Fatalf("session close count = %d, want 1", got)
	}
}
