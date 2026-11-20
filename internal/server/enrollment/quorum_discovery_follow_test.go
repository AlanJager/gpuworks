package enrollment

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func quorumDiscoveryPeerEndpoint(t *testing.T, f *fixture, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	certificate, key := catalogMember(t, f)
	pair, err := tls.X509KeyPair(append(certificate, f.ca...), key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("invalid CA")
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, fingerprint
}

func TestQuorumDiscoveryFollowsSeparateLeaderAndBindsCandidate(t *testing.T) {
	for _, name := range []string{"valid", "candidate address changed", "candidate ID changed", "wrong provider key", "no voter origin", "only spare origin", "provider unavailable", "provider old capability", "provider nonce", "provider leader key"} {
		t.Run(name, func(t *testing.T) {
			var f *fixture
			var contact string
			var leader *httptest.Server
			var leaderKey string
			var discoveryCalls, factsCalls atomic.Int32
			discoveryNonces := make(chan string, 1)
			f, contact = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				discoveryCalls.Add(1)
				nonce := quorumDiscoveryClientRequest(t, r)
				discoveryNonces <- nonce
				candidate := ClusterMember{ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: leaderKey, EnrollmentOrigin: leader.URL}
				switch name {
				case "candidate address changed":
					candidate.Address = "127.0.0.1:9020"
				case "candidate ID changed":
					candidate.ID = 10
				case "wrong provider key":
					candidate.KeyFingerprint = strings.Repeat("0", 64)
				case "no voter origin":
					candidate.EnrollmentOrigin = ""
				case "only spare origin":
					candidate.Role = "spare"
				}
				hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce, Members: []ClusterMember{candidate, {ID: 12, Address: "127.0.0.1:9011", Role: "voter", KeyFingerprint: contact}}}
				data, err := json.Marshal(hints)
				if err != nil {
					t.Error(err)
				}
				_, _ = w.Write(data)
			}))
			leader, leaderKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				factsCalls.Add(1)
				nonce := quorumFactsClientRequest(t, r)
				if nonce == <-discoveryNonces {
					t.Error("facts reused discovery nonce")
				}
				payload := quorumFactsClientResponse(f, leaderKey, nonce)
				switch name {
				case "provider unavailable":
					w.WriteHeader(503)
				case "provider old capability":
					w.WriteHeader(404)
				case "provider nonce":
					payload = strings.Replace(payload, nonce, strings.Repeat("0", 64), 1)
				case "provider leader key":
					payload = strings.Replace(payload, `"leader_id":"11"`, `"leader_id":"12"`, 1)
				}
				_, _ = io.WriteString(w, payload)
			}))
			cert, key := catalogMember(t, f)
			got, err := f.client(t).DiscoverClusterQuorumFacts(t.Context(), cert, key, []string{contact})
			if name == "valid" {
				if err != nil || got.LeaderID != 11 || got.Members[0].KeyFingerprint != leaderKey || got.Phase != "frozen" {
					t.Fatalf("separate leader not resolved: %+v %v", got, err)
				}
			} else {
				assertQuorumFactsClientZero(t, got, err)
			}
			wantFacts := int32(1)
			if name == "wrong provider key" || name == "no voter origin" || name == "only spare origin" {
				wantFacts = 0
			}
			if discoveryCalls.Load() != 1 || factsCalls.Load() != wantFacts {
				t.Fatalf("unexpected grant/fallback requests: discovery=%d facts=%d", discoveryCalls.Load(), factsCalls.Load())
			}
		})
	}
}

func TestQuorumDiscoveryFollowerFailureAllowsNextExplicitVoter(t *testing.T) {
	var f *fixture
	var contact string
	var leader *httptest.Server
	var leaderKey string
	var discovery, followerFacts, leaderFacts atomic.Int32
	followerEntered := make(chan struct{})
	f, contact = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ClusterQuorumFactsPath {
			followerFacts.Add(1)
			quorumFactsClientRequest(t, r)
			close(followerEntered)
			w.WriteHeader(503)
			return
		}
		discovery.Add(1)
		nonce := quorumDiscoveryClientRequest(t, r)
		hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce, Members: []ClusterMember{{ID: 10, Address: "127.0.0.1:9011", Role: "voter", KeyFingerprint: contact, EnrollmentOrigin: f.server.URL}, {ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: leaderKey, EnrollmentOrigin: leader.URL}}}
		data, err := json.Marshal(hints)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(data)
	}))
	leader, leaderKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaderFacts.Add(1)
		select {
		case <-followerEntered:
		case <-r.Context().Done():
			return
		}
		nonce := quorumFactsClientRequest(t, r)
		_, _ = io.WriteString(w, quorumFactsClientResponse(f, leaderKey, nonce))
	}))
	cert, key := catalogMember(t, f)
	got, err := f.client(t).DiscoverClusterQuorumFacts(t.Context(), cert, key, []string{contact})
	if err != nil || got.LeaderID != 11 || discovery.Load() != 1 || followerFacts.Load() != 1 || leaderFacts.Load() != 1 {
		t.Fatalf("explicit voter sequence failed: %+v %v counts=%d/%d/%d", got, err, discovery.Load(), followerFacts.Load(), leaderFacts.Load())
	}
}

func TestQuorumDiscoveryWholeOperationParentDeadline(t *testing.T) {
	var f *fixture
	var contact string
	var leader *httptest.Server
	var leaderKey string
	entered := make(chan struct{})
	f, contact = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := quorumDiscoveryClientRequest(t, r)
		hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce, Members: []ClusterMember{{ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: leaderKey, EnrollmentOrigin: leader.URL}, {ID: 12, Address: "127.0.0.1:9011", Role: "spare", KeyFingerprint: contact}}}
		data, err := json.Marshal(hints)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(data)
	}))
	leader, leaderKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quorumFactsClientRequest(t, r)
		close(entered)
		<-r.Context().Done()
	}))
	cert, key := catalogMember(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	got, err := f.client(t).DiscoverClusterQuorumFacts(ctx, cert, key, []string{contact})
	assertQuorumFactsClientZero(t, got, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lost total deadline", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("deadline occurred before leader fixture")
	}
}

func TestQuorumDiscoveryHeldFirstVoterDoesNotBlockHealthyLaterVoter(t *testing.T) {
	var f *fixture
	var contact string
	var leader *httptest.Server
	var leaderKey string
	var discoveryCalls, heldCalls, leaderCalls atomic.Int32
	heldEntered := make(chan struct{})
	heldReturned := make(chan struct{})
	f, contact = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ClusterQuorumFactsPath {
			heldCalls.Add(1)
			quorumFactsClientRequest(t, r)
			close(heldEntered)
			<-r.Context().Done()
			close(heldReturned)
			return
		}
		discoveryCalls.Add(1)
		nonce := quorumDiscoveryClientRequest(t, r)
		hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce, Members: []ClusterMember{
			{ID: 10, Address: "127.0.0.1:9011", Role: "voter", KeyFingerprint: contact, EnrollmentOrigin: f.server.URL},
			{ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: leaderKey, EnrollmentOrigin: leader.URL},
		}}
		data, err := json.Marshal(hints)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(data)
	}))
	leader, leaderKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaderCalls.Add(1)
		nonce := quorumFactsClientRequest(t, r)
		select {
		case <-heldEntered:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, quorumFactsClientResponse(f, leaderKey, nonce))
	}))
	cert, key := catalogMember(t, f)
	// The held voter must not consume this total deadline before the healthy voter.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	got, err := f.client(t).DiscoverClusterQuorumFacts(ctx, cert, key, []string{contact})
	if err != nil || got.LeaderID != 11 || ctx.Err() != nil {
		t.Fatalf("held first voter starved healthy later voter: %+v %v", got, err)
	}
	// The TLS server acknowledges its own handler exit. Client worker joining is
	// local completion; arbitrary remote handler drain requires a remote protocol.
	select {
	case <-heldReturned:
	case <-ctx.Done():
		t.Fatal("held HTTP request did not observe cancellation and return")
	}
	if discoveryCalls.Load() != 1 || heldCalls.Load() != 1 || leaderCalls.Load() != 1 {
		t.Fatalf("unexpected requests: %d/%d/%d", discoveryCalls.Load(), heldCalls.Load(), leaderCalls.Load())
	}
}
