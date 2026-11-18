package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAppCloseWaitsForRequestsAndClosesExactlyOnce(t *testing.T) {
	a, _ := newTestApp(t, "gpuworks-test-password-2026")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	a.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(context.Background()) }()
	deadline := time.Now().Add(3 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.Addr() == "127.0.0.1:0" {
		t.Fatal("Run did not bind")
	}
	responseDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + a.Addr() + "/")
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = errors.New("unexpected request status")
			}
		}
		responseDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never entered")
	}
	const closers = 6
	closeDone := make(chan error, closers)
	for range closers {
		go func() { closeDone <- a.Close() }()
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		err := a.Run(context.Background())
		if errors.Is(err, ErrClosed) {
			break
		}
		if err == nil || time.Now().After(deadline) {
			t.Fatalf("Run while closing = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned under an in-flight HTTP request: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	for range closers {
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not finish")
		}
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CountNodes(); err == nil {
		t.Error("main database remains usable after final Close")
	}
	if err := a.obs.UX().Sync(); err == nil {
		t.Error("request writer remains open after final Close")
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Run = %v", err)
	}
	if err := a.SetAddr("127.0.0.1:0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed SetAddr = %v", err)
	}
	// Reopening the same installation proves final Close released ownership of
	// the file, rather than merely closing the listener.
	reopened, err := New(a.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	defer reopened.Close()
}

func TestFailedBindDoesNotDestroyTheAssembledApp(t *testing.T) {
	a, _ := newTestApp(t, "gpuworks-test-password-2026")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := a.SetAddr(listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("Run unexpectedly bound an occupied port")
	}
	if _, err := a.store.CountNodes(); err != nil {
		t.Fatalf("bind failure closed the App store: %v", err)
	}
	listener.Close()
	if err := a.SetAddr("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.Addr() == "127.0.0.1:0" {
		cancel()
		t.Fatal("App could not Run after bind failure")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type failedLifetimeInit struct {
	t         *testing.T
	st        *store.Store
	baselines map[string]int
	failure   error
}

func (*failedLifetimeInit) Name() string { return "failed_init" }
func (m *failedLifetimeInit) Init(_ context.Context, h *Host) error {
	m.st = h.st
	for marker, count := range m.baselines {
		waitForGoroutineCount(m.t, marker, count+1, 3*time.Second)
	}
	return m.failure
}

func TestFailedAssemblyReleasesEveryOpenedResource(t *testing.T) {
	markers := map[string]int{}
	for _, marker := range []string{"internal/server/uxstore.(*Store).run(", "internal/server/traceexport.(*Exporter).run("} {
		markers[marker] = countGoroutines(marker)
	}
	failure := errors.New("controlled extension initialization failure")
	module := &failedLifetimeInit{t: t, baselines: markers, failure: failure}
	dir := t.TempDir()
	cfg := Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026", Extensions: Extensions{Modules: []Module{module}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(cfg, log); !errors.Is(err, failure) {
		t.Fatalf("New failure=%v", err)
	}
	if module.st == nil {
		t.Fatal("failure was not injected after opening resources")
	}
	if _, err := module.st.CountNodes(); err == nil {
		t.Error("failed New left its main store open")
	}
	for marker, count := range markers {
		if got := countGoroutines(marker); got != count {
			t.Errorf("failed New leaked %s: %d vs %d", marker, got, count)
		}
	}
	cfg.Extensions = Extensions{}
	a, err := New(cfg, log)
	if err != nil {
		t.Fatalf("retry after failed New: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFinalCloseWaitsForHandlerAfterHTTPGraceExpires(t *testing.T) {
	a, _ := newTestApp(t, "gpuworks-test-password-2026")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	a.http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release // Simulate a store operation that cannot observe client cancellation.
	})
	go func() { _ = a.Run(context.Background()) }()
	deadline := time.Now().Add(3 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.Addr() == "127.0.0.1:0" {
		t.Fatal("Run did not bind")
	}
	clientDone := make(chan struct{})
	go func() {
		response, _ := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + a.Addr() + "/")
		if response != nil {
			response.Body.Close()
		}
		close(clientDone)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case <-clientDone: // Shutdown's grace expired and the connection was closed.
	case <-time.After(8 * time.Second):
		t.Fatal("HTTP connection did not close after the shutdown grace period")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close released stores under a running handler: %v", err)
	case <-time.After(3 * time.Second):
	}
	if _, err := a.store.CountNodes(); err != nil {
		t.Fatalf("handler's store closed before it returned: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish after the handler returned")
	}
}
