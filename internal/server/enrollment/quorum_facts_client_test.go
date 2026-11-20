package enrollment

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Reuse the real cluster CA and server leaf fixture, with client authentication
// configured before the replacement TLS listener starts.
func quorumFactsClientEndpoint(t *testing.T, handler http.Handler) (*fixture, string) {
	t.Helper()
	f := endpoint(t, handler)
	f.server.Close()
	config := f.server.TLS.Clone()
	config.ClientAuth = tls.RequireAndVerifyClientCert
	config.ClientCAs = x509.NewCertPool()
	if !config.ClientCAs.AppendCertsFromPEM(f.ca) {
		t.Fatal("fixture CA rejected")
	}
	f.server = httptest.NewUnstartedServer(handler)
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.TLS = config
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	leaf, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return f, hex.EncodeToString(sum[:])
}

func quorumFactsClientRequest(t *testing.T, r *http.Request) string {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/_cluster/quorum-facts" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
		t.Error("request departed from fixed read-only quorum facts contract")
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || len(r.TLS.VerifiedChains) == 0 {
		t.Error("request did not authenticate its actual member certificate")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		t.Error(err)
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 1 {
		t.Error("request must contain exactly one nonce field")
		return ""
	}
	var nonce string
	if err := json.Unmarshal(fields["nonce"], &nonce); err != nil {
		t.Error("missing nonce string")
	}
	decoded, err := hex.DecodeString(nonce)
	if err != nil || len(decoded) != 32 || nonce != strings.ToLower(nonce) || string(data) != `{"nonce":"`+nonce+`"}` {
		t.Error("request nonce is not a canonical 32-byte random value")
	}
	return nonce
}

func quorumFactsClientResponse(f *fixture, provider, nonce string) string {
	// Literal wire response avoids accepting accidental DTO serialization changes.
	return fmt.Sprintf(`{"cluster_id":%q,"ca_fingerprint":%q,"protocol":1,"nonce":%q,"legacy":false,"phase":"frozen","phase_revision":"37","term":"7","index":"23","configuration_index":"20","leader_id":"11","members":[{"id":"11","address":"127.0.0.1:9010","role":"voter","key_fingerprint":%q},{"id":"12","address":"other.example:9010","role":"voter","key_fingerprint":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]}`, f.cluster, f.pin, nonce, provider)
}

func assertQuorumFactsClientZero(t *testing.T, got ClusterQuorumFacts, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(got, ClusterQuorumFacts{}) {
		t.Fatalf("rejection returned partial observation: facts=%+v err=%v", got, err)
	}
}

func TestQuorumFactsClientReadsLiteralSnapshotWithFreshNonce(t *testing.T) {
	var f *fixture
	var provider string
	nonces := make(chan string, 2)
	var requests atomic.Int32
	f, provider = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		nonce := quorumFactsClientRequest(t, r)
		nonces <- nonce
		_, _ = io.WriteString(w, quorumFactsClientResponse(f, provider, nonce))
	}))
	cert, key := catalogMember(t, f)
	client := f.client(t)
	var previousNonce string
	for range 2 {
		got, err := client.FetchClusterQuorumFacts(t.Context(), cert, key, provider)
		nonce := <-nonces
		want := ClusterQuorumFacts{
			ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce,
			Phase: "frozen", PhaseRevision: 37, Term: 7, Index: 23, ConfigurationIndex: 20, LeaderID: 11,
			Members: []ClusterMember{
				{ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: provider},
				{ID: 12, Address: "other.example:9010", Role: "voter", KeyFingerprint: strings.Repeat("e", 64)},
			},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("literal observation rejected or altered: got=%+v err=%v", got, err)
		}
		if len(nonces) != 0 {
			t.Fatal("unexpected extra request")
		}
		if nonce == "" || nonce == previousNonce {
			t.Fatal("successive observations reused or omitted their request nonce")
		}
		previousNonce = nonce
	}
	if requests.Load() != 2 {
		t.Fatal("quorum read issued extra requests or authorization grant fallback")
	}
}

func TestObservedQuorumFactsKeepsTheLeaderThatServedTheSnapshot(t *testing.T) {
	var f *fixture
	var provider string
	f, provider = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := quorumFactsClientRequest(t, r)
		_, _ = io.WriteString(w, quorumFactsClientResponse(f, provider, nonce))
	}))
	cert, key := catalogMember(t, f)
	got, err := f.client(t).FetchObservedQuorumFacts(t.Context(), cert, key)
	if err != nil || got.LeaderID != 11 || got.Term != 7 || got.Index != 23 || got.ConfigurationIndex != 20 || len(got.Members) != 2 {
		t.Fatalf("observed facts=%+v err=%v", got, err)
	}
}

func TestObservedQuorumFactsRejectsALeaderKeyThatDidNotServe(t *testing.T) {
	var f *fixture
	f, _ = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := quorumFactsClientRequest(t, r)
		_, _ = io.WriteString(w, quorumFactsClientResponse(f, strings.Repeat("c", 64), nonce))
	}))
	cert, key := catalogMember(t, f)
	got, err := f.client(t).FetchObservedQuorumFacts(t.Context(), cert, key)
	assertQuorumFactsClientZero(t, got, err)
	if err == nil || !strings.Contains(err.Error(), "not the observed voter leader") {
		t.Fatal(err)
	}
}

func TestQuorumFactsClientRejectsUnboundProviderBeforeRequest(t *testing.T) {
	for _, name := range []string{"missing", "noncanonical", "different peer", "wrong CA", "wrong client key", "pre-canceled"} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			f, provider := quorumFactsClientEndpoint(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			cert, key := catalogMember(t, f)
			client := f.client(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "missing":
				provider = ""
			case "noncanonical":
				provider = strings.Repeat("A", 64)
			case "different peer":
				provider = strings.Repeat("0", 64)
			case "wrong CA":
				var err error
				client, err = NewClient(f.server.URL+certificatePath, f.cluster, strings.Repeat("0", 64), 1)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(client.Close)
			case "wrong client key":
				key = []byte("invalid private key")
			case "pre-canceled":
				cancel()
			}
			got, err := client.FetchClusterQuorumFacts(ctx, cert, key, provider)
			assertQuorumFactsClientZero(t, got, err)
			if name == "pre-canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause lost", err)
			}
			if requests.Load() != 0 {
				t.Fatal("unbound request reached HTTP handler")
			}
		})
	}
}

func TestQuorumFactsClientRejectsRemoteObservationWithoutFallback(t *testing.T) {
	for _, name := range []string{"cluster", "CA", "protocol", "nonce", "leader key", "leader role", "old capability", "unavailable", "malformed", "oversize", "partial body", "redirect"} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			var redirects atomic.Int32
			redirect := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
			defer redirect.Close()
			var f *fixture
			var provider string
			f, provider = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				nonce := quorumFactsClientRequest(t, r)
				payload := quorumFactsClientResponse(f, provider, nonce)
				switch name {
				case "cluster":
					payload = strings.Replace(payload, f.cluster, strings.Repeat("b", 32), 1)
				case "CA":
					payload = strings.Replace(payload, f.pin, strings.Repeat("b", 64), 1)
				case "protocol":
					payload = strings.Replace(payload, `"protocol":1`, `"protocol":2`, 1)
				case "nonce":
					payload = strings.Replace(payload, nonce, strings.Repeat("0", 64), 1)
				case "leader key":
					payload = strings.Replace(payload, `"leader_id":"11"`, `"leader_id":"12"`, 1)
				case "leader role":
					payload = strings.Replace(payload, `"role":"voter"`, `"role":"spare"`, 1)
				case "old capability":
					w.WriteHeader(http.StatusNotFound)
				case "unavailable":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "malformed":
					payload = `{"private-response-marker":`
				case "oversize":
					payload = strings.Repeat("x", (512<<10)+1)
				case "partial body":
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)+20))
				case "redirect":
					w.Header().Set("Location", redirect.URL)
					w.WriteHeader(http.StatusTemporaryRedirect)
				}
				_, _ = io.WriteString(w, payload)
			}))
			cert, key := catalogMember(t, f)
			got, err := f.client(t).FetchClusterQuorumFacts(t.Context(), cert, key, provider)
			assertQuorumFactsClientZero(t, got, err)
			if strings.Contains(err.Error(), "private-response-marker") {
				t.Fatal("remote response leaked in error")
			}
			if requests.Load() != 1 || redirects.Load() != 0 {
				t.Fatal("rejected observation triggered fallback or redirect")
			}
		})
	}
}

func TestQuorumFactsClientCancellationBoundsHeadersAndBody(t *testing.T) {
	for _, name := range []string{"cancel headers", "cancel body", "parent deadline headers", "parent deadline body", "default five second bound"} {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			returned := make(chan struct{})
			f, provider := quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(returned)
				_ = quorumFactsClientRequest(t, r)
				if strings.Contains(name, "body") {
					w.Header().Set("Content-Length", "10000")
					_, _ = io.WriteString(w, "{")
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
			}))
			cert, key := catalogMember(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if strings.HasPrefix(name, "parent deadline") {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer deadlineCancel()
			}
			type result struct {
				facts ClusterQuorumFacts
				err   error
			}
			done := make(chan result, 1)
			client := f.client(t)
			started := time.Now()
			go func() {
				got, err := client.FetchClusterQuorumFacts(ctx, cert, key, provider)
				done <- result{got, err}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("request did not reach authenticated endpoint")
			}
			wantCause := context.DeadlineExceeded
			if strings.HasPrefix(name, "cancel") {
				wantCause = context.Canceled
				cancel()
			}
			limit := 2 * time.Second
			if name == "default five second bound" {
				limit = 6 * time.Second
			}
			select {
			case got := <-done:
				assertQuorumFactsClientZero(t, got.facts, got.err)
				if !errors.Is(got.err, wantCause) {
					t.Fatal("actual context cause lost", got.err)
				}
				if name == "default five second bound" && time.Since(started) < 4500*time.Millisecond {
					t.Fatal("fixture failed before exercising the five second deadline")
				}
			case <-time.After(limit):
				cancel()
				t.Fatal("quorum facts ignored parent or five second deadline")
			}
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("actual HTTP request remained active after cancellation")
			}
		})
	}
}
