//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

type statusDiscoveryFixture struct {
	status                              clusterStatus
	self, leader, seed                  *httptest.Server
	selfKey, leaderKey                  string
	seedCalls, discoveries, leaderFacts atomic.Int32
}

func newStatusDiscoveryFixture(t *testing.T, fault string) *statusDiscoveryFixture {
	t.Helper()
	f := &statusDiscoveryFixture{}
	cluster := strings.Repeat("a", 32)
	ca, caKey, err := identity.NewAuthority(cluster, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := identity.LoadAuthority(ca, caKey, cluster, pin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA")
	}
	member := func() ([]byte, []byte, string) {
		key, err := identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := identity.NewMemberCSR(key)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := identity.CSRKeyFingerprint(csr)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := issuer.IssueMember(csr, fp, "127.0.0.1:9443", time.Now(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key, fp
	}
	selfCert, selfPrivate, selfFP := member()
	leaderCert, leaderPrivate, leaderFP := member()
	f.selfKey, f.leaderKey = selfFP, leaderFP
	start := func(cert, key []byte, handler http.HandlerFunc) *httptest.Server {
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(handler)
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	requestNonce := func(r *http.Request, path string) string {
		if r.Method != http.MethodPost || r.URL.Path != path || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Errorf("unauthenticated or unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return ""
		}
		request, err := enrollment.DecodeClusterQuorumFactsRequest(body)
		if err != nil {
			t.Error(err)
			return ""
		}
		return request.Nonce
	}
	members := func() []enrollment.ClusterMember {
		return []enrollment.ClusterMember{
			{ID: 1, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: selfFP, EnrollmentOrigin: f.self.URL},
			{ID: 2, Address: "127.0.0.1:9011", Role: "voter", KeyFingerprint: leaderFP, EnrollmentOrigin: f.leader.URL},
		}
	}
	f.leader = start(leaderCert, leaderPrivate, func(w http.ResponseWriter, r *http.Request) {
		f.leaderFacts.Add(1)
		nonce := requestNonce(r, enrollment.ClusterQuorumFactsPath)
		if fault == "deadline" || fault == "cancel" {
			<-r.Context().Done()
			return
		}
		facts := enrollment.ClusterQuorumFacts{ClusterID: cluster, CAFingerprint: pin, Protocol: 1, Nonce: nonce, Phase: "frozen", PhaseRevision: 5, Term: 7, Index: 20, ConfigurationIndex: 4, LeaderID: 2, Members: members()}
		switch fault {
		case "facts nonce":
			facts.Nonce = strings.Repeat("0", 64)
		case "fake leader":
			facts.LeaderID = 1
		case "candidate ID":
			facts.Members[1].ID, facts.LeaderID = 3, 3
		case "candidate address":
			facts.Members[1].Address = "127.0.0.1:9020"
		}
		_ = json.NewEncoder(w).Encode(facts)
	})
	selfServerCert, selfServerPrivate := selfCert, selfPrivate
	if fault == "contact TLS key" {
		selfServerCert, selfServerPrivate = leaderCert, leaderPrivate
	}
	f.self = start(selfServerCert, selfServerPrivate, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == enrollment.ClusterQuorumFactsPath {
			requestNonce(r, enrollment.ClusterQuorumFactsPath)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.discoveries.Add(1)
		nonce := requestNonce(r, enrollment.ClusterQuorumDiscoveryPath)
		if fault == "old capability" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hints := enrollment.ClusterQuorumDiscovery{ClusterID: cluster, CAFingerprint: pin, Protocol: 1, Nonce: nonce, Members: members()}
		if fault == "discovery nonce" {
			hints.Nonce = strings.Repeat("0", 64)
		}
		if fault == "provider key" {
			hints.Members[1].KeyFingerprint = strings.Repeat("0", 64)
		}
		if fault == "contact absent" {
			hints.Members[0].KeyFingerprint = strings.Repeat("e", 64)
		}
		_ = json.NewEncoder(w).Encode(hints)
	})
	f.seed = start(leaderCert, leaderPrivate, func(w http.ResponseWriter, r *http.Request) {
		f.seedCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	f.status = clusterStatus{clusterID: cluster, pin: pin, protocol: 1, cert: selfCert, key: selfPrivate, publicURL: f.self.URL, joinURL: f.seed.URL + enrollmentCertificatePath, id: 1}
	return f
}

func TestStatusQuorumDiscoversNewLeaderBeforeUnavailableOriginalSeed(t *testing.T) {
	f := newStatusDiscoveryFixture(t, "")
	facts, ok := fetchStatusQuorum(t.Context(), f.status, time.Second)
	if !ok || facts.LeaderID != 2 || facts.Term != 7 || facts.Index != 20 || facts.ConfigurationIndex != 4 {
		t.Fatalf("fresh leader observation missing: %+v, observed=%v", facts, ok)
	}
	if f.seedCalls.Load() != 0 || f.discoveries.Load() != 1 || f.leaderFacts.Load() != 1 {
		t.Fatalf("unexpected requests: seed=%d discovery=%d leader=%d", f.seedCalls.Load(), f.discoveries.Load(), f.leaderFacts.Load())
	}
	f.status.applyQuorumFacts(facts)
	if f.status.roleSource != "live raft" || f.status.role != "voter" || f.status.leader != "2 127.0.0.1:9011" || f.status.sync != "observed" {
		t.Fatalf("leader observation not reflected in status: %+v", f.status)
	}
}

func TestStatusQuorumDiscoveryRejectsUnboundFacts(t *testing.T) {
	for _, fault := range []string{"discovery nonce", "provider key", "contact absent", "contact TLS key", "facts nonce", "fake leader", "candidate ID", "candidate address"} {
		t.Run(fault, func(t *testing.T) {
			f := newStatusDiscoveryFixture(t, fault)
			// No fallback provider can turn a rejected discovery into success.
			f.status.joinURL = ""
			facts, ok := fetchStatusQuorum(t.Context(), f.status, time.Second)
			if ok || facts.LeaderID != 0 || len(facts.Members) != 0 {
				t.Fatalf("unbound facts accepted: %+v", facts)
			}
			wantDiscovery := int32(1)
			if fault == "contact TLS key" {
				wantDiscovery = 0
			}
			if f.discoveries.Load() != wantDiscovery {
				t.Fatalf("discovery did not run: %d", f.discoveries.Load())
			}
		})
	}
}

func TestStatusQuorumPreservesDirectLeaderAndOriginalSeedFallback(t *testing.T) {
	for _, mode := range []string{"direct leader", "original seed", "listen origin"} {
		t.Run(mode, func(t *testing.T) {
			f := newStatusDiscoveryFixture(t, "old capability")
			f.status.joinURL = f.leader.URL + enrollmentCertificatePath
			wantDiscovery := int32(1)
			switch mode {
			case "direct leader":
				f.status.publicURL = f.leader.URL
				wantDiscovery = 0
			case "listen origin":
				f.status.publicURL = ""
				f.status.enrollmentListen = strings.TrimPrefix(f.self.URL, "https://")
			}
			facts, ok := fetchStatusQuorum(t.Context(), f.status, time.Second)
			if !ok || facts.LeaderID != 2 || f.leaderFacts.Load() != 1 || f.discoveries.Load() != wantDiscovery {
				t.Fatalf("fallback rejected: observed=%v facts=%+v leader=%d discovery=%d", ok, facts, f.leaderFacts.Load(), f.discoveries.Load())
			}
		})
	}
}

func TestStatusQuorumDiscoveryDeadlineAndCancellationRemainUnobserved(t *testing.T) {
	for _, fault := range []string{"deadline", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newStatusDiscoveryFixture(t, fault)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fault == "cancel" {
				go func() {
					for f.leaderFacts.Load() == 0 && ctx.Err() == nil {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
			}
			started := time.Now()
			facts, ok := fetchStatusQuorum(ctx, f.status, 200*time.Millisecond)
			if ok || facts.LeaderID != 0 || len(facts.Members) != 0 {
				t.Fatalf("timed out observation accepted: %+v", facts)
			}
			if time.Since(started) > time.Second || f.leaderFacts.Load() != 1 || f.seedCalls.Load() != 0 {
				t.Fatalf("discovery was not bounded: elapsed=%s leader=%d seed=%d", time.Since(started), f.leaderFacts.Load(), f.seedCalls.Load())
			}
		})
	}
}
