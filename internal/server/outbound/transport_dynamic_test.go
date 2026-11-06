package outbound

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestPrivateHostChangesApplyToAnExistingClient(t *testing.T) {
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "archive")
	}))
	defer origin.Close()
	var hosts []string
	transport := TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, func() []string { return hosts })
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	read := func(allowed bool) {
		t.Helper()
		response, err := client.Get(origin.URL)
		if !allowed {
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrTargetDenied) {
				t.Fatalf("revoked/private target: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil || string(body) != "archive" {
			t.Fatalf("body=%q read=%v close=%v", body, err, closeErr)
		}
	}
	read(false)
	hosts = []string{"127.0.0.1"}
	read(true)
	read(true) // Leave a reusable connection in this same client's pool.
	hosts = nil
	read(false)
	if hits.Load() != 2 {
		t.Fatalf("unapproved request reached origin: hits=%d", hits.Load())
	}
}

func TestDynamicPrivatePermissionCannotAuthorizeMetadataRedirects(t *testing.T) {
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer origin.Close()
	transport := TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, func() []string { return []string{"0.0.0.0/0", "::/0"} })
	defer transport.CloseIdleConnections()
	_, err := (&http.Client{Transport: transport}).Get(origin.URL)
	if !errors.Is(err, ErrTargetDenied) || hits.Load() != 1 {
		t.Fatalf("metadata redirect: hits=%d err=%v", hits.Load(), err)
	}
}

func TestRevocationBetweenRedirectHopsAppliesImmediately(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed.Store(false)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	transport := TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, func() []string {
		if allowed.Load() {
			return []string{"127.0.0.1"}
		}
		return nil
	})
	defer transport.CloseIdleConnections()
	_, err := (&http.Client{Transport: transport}).Get(origin.URL)
	if !errors.Is(err, ErrTargetDenied) || targetHits.Load() != 0 {
		t.Fatalf("revoked redirect: target hits=%d err=%v", targetHits.Load(), err)
	}
}

func TestRevokingPermissionDoesNotInterruptAnAcceptedResponseBody(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	finish := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "head")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			_, _ = io.WriteString(w, "tail")
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, func() []string {
		if allowed.Load() {
			return []string{"127.0.0.1"}
		}
		return nil
	})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	allowed.Store(false)
	close(finish)
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "headtail" {
		t.Fatalf("accepted body was interrupted: %q %v", body, err)
	}
	if _, err := client.Get(origin.URL); !errors.Is(err, ErrTargetDenied) {
		t.Fatalf("request after revocation: %v", err)
	}
}
