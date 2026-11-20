package enrollment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quorumDiscoveryClientRequest(t *testing.T, r *http.Request) string {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != ClusterQuorumDiscoveryPath || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/json" {
		t.Error("discovery departed from fixed read-only request")
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || len(r.TLS.VerifiedChains) == 0 {
		t.Error("missing member TLS authentication")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		t.Error(err)
		return ""
	}
	request, err := DecodeClusterQuorumFactsRequest(data)
	if err != nil || string(data) != `{"nonce":"`+request.Nonce+`"}` {
		t.Error("request nonce is not exact canonical wire", err)
	}
	return request.Nonce
}

func quorumDiscoveryClientResponse(f *fixture, peer, nonce string) string {
	return fmt.Sprintf(`{"cluster_id":%q,"ca_fingerprint":%q,"protocol":1,"nonce":%q,"members":[{"id":"11","address":"127.0.0.1:9010","role":"voter","key_fingerprint":%q}]}`, f.cluster, f.pin, nonce, peer)
}

func assertQuorumDiscoveryZero(t *testing.T, got ClusterQuorumDiscovery, peer string, err error) {
	t.Helper()
	if err == nil || peer != "" || !reflect.DeepEqual(got, ClusterQuorumDiscovery{}) {
		t.Fatalf("partial discovery: %+v %q %v", got, peer, err)
	}
}

func TestQuorumDiscoveryClientFreshNonceAndAllowedPeer(t *testing.T) {
	var f *fixture
	var peer string
	nonces := make(chan string, 2)
	var calls atomic.Int32
	f, peer = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		nonce := quorumDiscoveryClientRequest(t, r)
		nonces <- nonce
		_, _ = io.WriteString(w, quorumDiscoveryClientResponse(f, peer, nonce))
	}))
	cert, key := catalogMember(t, f)
	client := f.client(t)
	previous := ""
	for range 2 {
		got, actual, err := client.FetchClusterQuorumDiscovery(t.Context(), cert, key, []string{peer})
		nonce := <-nonces
		if err != nil || actual != peer || got.Nonce != nonce || nonce == previous || len(got.Members) != 1 || got.Members[0].KeyFingerprint != peer {
			t.Fatalf("unexpected hints: %+v %q %v", got, actual, err)
		}
		previous = nonce
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected fallback")
	}
}

func TestQuorumDiscoveryClientRejectsContactBeforeHTTPRequest(t *testing.T) {
	for _, name := range []string{"missing", "uppercase", "unsorted", "duplicate", "too many", "different peer", "wrong client key", "pre-canceled"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			f, peer := quorumFactsClientEndpoint(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			cert, key := catalogMember(t, f)
			client := f.client(t)
			allowed := []string{peer}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "missing":
				allowed = nil
			case "uppercase":
				allowed = []string{strings.Repeat("A", 64)}
			case "unsorted":
				allowed = []string{strings.Repeat("f", 64), strings.Repeat("0", 64)}
			case "duplicate":
				allowed = []string{peer, peer}
			case "too many":
				allowed = make([]string, 513)
			case "different peer":
				allowed = []string{strings.Repeat("0", 64)}
			case "wrong client key":
				key = []byte("invalid")
			case "pre-canceled":
				cancel()
			}
			got, actual, err := client.FetchClusterQuorumDiscovery(ctx, cert, key, allowed)
			assertQuorumDiscoveryZero(t, got, actual, err)
			if name == "pre-canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation", err)
			}
			if calls.Load() != 0 {
				t.Fatal("disallowed contact received HTTP credentials")
			}
		})
	}
}

func TestQuorumDiscoveryClientRejectsRemoteHintsWithoutFallback(t *testing.T) {
	for _, name := range []string{"cluster", "CA", "protocol", "nonce", "absent contact", "old capability", "unavailable", "malformed", "oversize", "partial body", "redirect"} {
		t.Run(name, func(t *testing.T) {
			var calls, redirects atomic.Int32
			redirect := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
			defer redirect.Close()
			var f *fixture
			var peer string
			f, peer = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				nonce := quorumDiscoveryClientRequest(t, r)
				payload := quorumDiscoveryClientResponse(f, peer, nonce)
				switch name {
				case "cluster":
					payload = strings.Replace(payload, f.cluster, strings.Repeat("b", 32), 1)
				case "CA":
					payload = strings.Replace(payload, f.pin, strings.Repeat("b", 64), 1)
				case "protocol":
					payload = strings.Replace(payload, `"protocol":1`, `"protocol":2`, 1)
				case "nonce":
					payload = strings.Replace(payload, nonce, strings.Repeat("0", 64), 1)
				case "absent contact":
					payload = strings.Replace(payload, peer, strings.Repeat("0", 64), 1)
				case "old capability":
					w.WriteHeader(404)
				case "unavailable":
					w.WriteHeader(503)
				case "malformed":
					payload = `{"nonce":`
				case "oversize":
					payload = strings.Repeat("x", (512<<10)+1)
				case "partial body":
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)+20))
				case "redirect":
					w.Header().Set("Location", redirect.URL)
					w.WriteHeader(307)
				}
				_, _ = io.WriteString(w, payload)
			}))
			cert, key := catalogMember(t, f)
			got, actual, err := f.client(t).FetchClusterQuorumDiscovery(t.Context(), cert, key, []string{peer})
			assertQuorumDiscoveryZero(t, got, actual, err)
			if calls.Load() != 1 || redirects.Load() != 0 {
				t.Fatal("extra request or redirected credential exchange")
			}
		})
	}
}

func TestQuorumDiscoveryClientCancellationAndParentDeadline(t *testing.T) {
	for _, name := range []string{"cancel", "deadline"} {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			var f *fixture
			var peer string
			f, peer = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				quorumDiscoveryClientRequest(t, r)
				close(entered)
				<-r.Context().Done()
			}))
			cert, key := catalogMember(t, f)
			var ctx context.Context
			var cancel context.CancelFunc
			if name == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), 300*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			if name == "cancel" {
				go func() { <-entered; cancel() }()
			}
			got, actual, err := f.client(t).FetchClusterQuorumDiscovery(ctx, cert, key, []string{peer})
			assertQuorumDiscoveryZero(t, got, actual, err)
			want := context.Canceled
			if name == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatal("lost parent context cause", err)
			}
		})
	}
}
