//go:build linux && dqlite

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func enrollmentListenerTLS(t *testing.T) (*tls.Config, *http.Client) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: fixture.TLS.Certificates}, &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func enrollmentListener(t *testing.T, addr string, handler http.Handler) (*enrollmentServer, *http.Client) {
	t.Helper()
	config, client := enrollmentListenerTLS(t)
	server, err := newEnrollmentServer(addr, handler, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	return server, client
}

func waitEnrollmentListener(t *testing.T, server *enrollmentServer) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("enrollment listener did not exit")
		return nil
	}
}

func assertEnrollmentRequest(client *http.Client, address string) error {
	response, err := client.Get("https://" + address + "/api/v1/cluster/invitations")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || string(body) != "enrollment" {
		return fmt.Errorf("unexpected HTTPS response: status=%d body=%q", response.StatusCode, body)
	}
	return nil
}

func TestEnrollmentServerReusesClosedListenerForPreparedRoutes(t *testing.T) {
	config, client := enrollmentListenerTLS(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	base, err := newEnrollmentServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/material" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), config, log)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := newEnrollmentServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/material" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/business" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	}), config, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := base.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = base.Close(shutdown)
	})
	address := base.Addr()
	status := func(path string) int {
		t.Helper()
		response, err := client.Get("https://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := status("/material"); got != http.StatusNoContent {
		t.Fatalf("closed material route: %d", got)
	}
	if got := status("/business"); got != http.StatusNotFound {
		t.Fatalf("closed business route: %d", got)
	}
	if err := base.adoptPreparedHandler(prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.run != nil || base.Addr() != address {
		t.Fatal("handler transfer started another listener or changed the bound address")
	}
	if got := status("/business"); got != http.StatusAccepted {
		t.Fatalf("prepared business route: %d", got)
	}
	if got := status("/material"); got != http.StatusNoContent {
		t.Fatalf("prepared material route: %d", got)
	}

	otherConfig := config.Clone()
	otherConfig.ClientAuth = tls.RequireAndVerifyClientCert
	other, err := newEnrollmentServer("127.0.0.1:0", http.NotFoundHandler(), otherConfig, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.adoptPreparedHandler(other); err == nil {
		t.Fatal("accepted routes prepared for a different TLS client policy")
	}
	base.materialReceiver = &materialReceiver{root: t.TempDir(), memberID: 7}
	prepared.materialReceiver = &materialReceiver{root: base.materialReceiver.root, memberID: 7}
	if err := base.adoptPreparedHandler(prepared); err == nil {
		t.Fatal("accepted routes with a different material receiver instance")
	}
	prepared.materialReceiver = base.materialReceiver
	if got := status("/business"); got != http.StatusAccepted {
		t.Fatalf("rejected transfer changed active routes: %d", got)
	}
	base.withdrawPreparedHandler()
	if got := status("/business"); got != http.StatusNotFound {
		t.Fatalf("withdrawn business route: %d", got)
	}
	if got := status("/material"); got != http.StatusNoContent {
		t.Fatalf("withdrawn material route: %d", got)
	}
	if err := base.adoptPreparedHandler(prepared); err != nil {
		t.Fatal(err)
	}
	if err := base.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := waitEnrollmentListener(t, base); err != nil {
		t.Fatal(err)
	}
	if err := base.Start(ctx); err != nil {
		t.Fatal(err)
	}
	address = base.Addr()
	if got := status("/business"); got != http.StatusNotFound {
		t.Fatalf("restarted listener retained prepared routes: %d", got)
	}
	if got := status("/material"); got != http.StatusNoContent {
		t.Fatalf("restarted material route: %d", got)
	}
}

func TestEnrollmentServerHTTPSCancellationAndRestart(t *testing.T) {
	var calls atomic.Int32
	server, client := enrollmentListener(t, "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.TLS == nil || !r.TLS.HandshakeComplete {
			t.Error("handler did not receive real TLS state")
		}
		_, _ = io.WriteString(w, "enrollment")
	}))
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := server.Wait(); err == nil {
		t.Fatal("Wait before Start did not report missing run")
	}
	for range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		if err := server.Start(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		address := server.Addr()
		if strings.HasSuffix(address, ":0") {
			t.Fatal("Start did not synchronously publish its port")
		}
		if err := assertEnrollmentRequest(client, address); err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		plain := &http.Client{Timeout: time.Second}
		response, err := plain.Get("http://" + address + "/api/v1/cluster/invitations")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("plaintext response status=%d", response.StatusCode)
			}
		}
		if calls.Load() != before {
			t.Fatal("plaintext request reached enrollment handler")
		}
		if err := server.Start(ctx); err == nil {
			t.Fatal("duplicate Start accepted")
		}
		cancel()
		if err := waitEnrollmentListener(t, server); err != nil {
			t.Fatal(err)
		}
		rebound, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("cancellation retained listener: %v", err)
		}
		rebound.Close()
	}
}

func TestEnrollmentServerBindFailureAndConcurrentStart(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	server, client := enrollmentListener(t, busy.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "enrollment") }))
	if err := server.Start(t.Context()); err == nil {
		t.Fatal("occupied port accepted")
	}
	if server.run != nil {
		t.Fatal("failed bind published a run")
	}
	busy.Close()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.Start(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Start: %v", err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if server.Start(t.Context()) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("concurrent accepted starts=%d", accepted.Load())
	}
	if err := assertEnrollmentRequest(client, server.Addr()); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := waitEnrollmentListener(t, server); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentServerCloseDrainsOrForcesActiveRequest(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server, client := enrollmentListener(t, "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				defer close(exited)
				select {
				case <-release:
					_, _ = io.WriteString(w, "enrollment")
				case <-r.Context().Done():
				}
			}))
			if err := server.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			requestDone := make(chan error, 1)
			go func() { requestDone <- assertEnrollmentRequest(client, server.Addr()) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("HTTPS request did not enter handler")
			}
			limit := time.Second
			if force {
				limit = 40 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), limit)
			defer cancel()
			closed := make(chan error, 1)
			go func() { closed <- server.Close(ctx) }()
			if !force {
				select {
				case err := <-closed:
					t.Fatalf("Close returned before active request drained: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				unblock()
			}
			err := <-closed
			if force && !errors.Is(err, context.DeadlineExceeded) || !force && err != nil {
				t.Fatalf("Close force=%t error=%v", force, err)
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("forced connection close did not cancel request")
			}
			if err := <-requestDone; force && err == nil || !force && err != nil {
				t.Fatalf("HTTPS request force=%t error=%v", force, err)
			}
			if err := waitEnrollmentListener(t, server); err != nil && (!force || !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal(err)
			}
		})
	}
}

func TestEnrollmentServerRejectsInvalidTLSConfiguration(t *testing.T) {
	config, _ := enrollmentListenerTLS(t)
	_, different, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"nil", "empty", "missing private key", "typed nil key", "short Ed25519 key", "mismatched key", "malformed certificate", "old TLS", "inverted TLS"} {
		t.Run(kind, func(t *testing.T) {
			candidate := config.Clone()
			candidate.Certificates = append([]tls.Certificate(nil), config.Certificates...)
			switch kind {
			case "nil":
				candidate = nil
			case "empty":
				candidate.Certificates = nil
			case "missing private key":
				candidate.Certificates[0].PrivateKey = nil
			case "typed nil key":
				candidate.Certificates[0].PrivateKey = (*rsa.PrivateKey)(nil)
			case "short Ed25519 key":
				candidate.Certificates[0].PrivateKey = ed25519.PrivateKey{1}
			case "mismatched key":
				candidate.Certificates[0].PrivateKey = different
			case "malformed certificate":
				candidate.Certificates[0].Certificate = [][]byte{[]byte("not DER")}
			case "old TLS":
				candidate.MinVersion = tls.VersionTLS10
			case "inverted TLS":
				candidate.MaxVersion = tls.VersionTLS12
			}
			if _, err := newEnrollmentServer("127.0.0.1:0", http.NotFoundHandler(), candidate, nil); err == nil {
				t.Fatal("invalid TLS configuration accepted")
			}
		})
	}
}

func TestEnrollmentServerImmediateCloseAndServeFailure(t *testing.T) {
	server, client := enrollmentListener(t, "127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "enrollment") }))
	for range 30 {
		if err := server.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := server.Close(ctx)
		cancel()
		if err != nil {
			t.Fatalf("close raced initial Serve: %v", err)
		}
		if err := waitEnrollmentListener(t, server); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := assertEnrollmentRequest(client, server.Addr()); err != nil {
		t.Fatal(err)
	}
	// An unexpected listener failure must also stop the run and reach Wait.
	if err := server.run.listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitEnrollmentListener(t, server); err == nil {
		t.Fatal("unexpected Serve failure was discarded")
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatalf("restart after Serve failure: %v", err)
	}
	if err := assertEnrollmentRequest(client, server.Addr()); err != nil {
		t.Fatal(err)
	}
}
