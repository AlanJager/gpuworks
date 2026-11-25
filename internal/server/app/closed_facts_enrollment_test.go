//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// This fixture models the fixed reader's identity/admission/context checks.
// Explicit bad results exercise the route's defensive validation separately.
type closedFactsRouteSession struct {
	mu            sync.Mutex
	expected      store.ClusterIdentity
	legacy        bool
	phase         store.MaintenancePhaseState
	admissions    []store.ClusterAdmission
	proof         cluster.QuorumProof
	err           error
	hold          func(context.Context) error
	badResult     bool
	calls         int
	seenIdentity  store.ClusterIdentity
	seenRequester string
	providerKey   string
}

func (s *closedFactsRouteSession) Close() error { return nil }

func (s *closedFactsRouteSession) ReadLocalQuorumFactsForCluster(ctx context.Context, expected store.ClusterIdentity, key string) (bool, store.MaintenancePhaseState, []store.ClusterAdmission, cluster.QuorumProof, error) {
	s.mu.Lock()
	s.calls++
	s.seenIdentity, s.seenRequester = expected, key
	legacy, phase, proof := s.legacy, s.phase, s.proof
	proof.Members = append([]cluster.Member(nil), proof.Members...)
	admissions := append([]store.ClusterAdmission(nil), s.admissions...)
	want, err, hold, badResult, providerKey := s.expected, s.err, s.hold, s.badResult, s.providerKey
	s.mu.Unlock()
	fail := func(err error) (bool, store.MaintenancePhaseState, []store.ClusterAdmission, cluster.QuorumProof, error) {
		return false, store.MaintenancePhaseState{}, nil, cluster.QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if expected.ClusterID != want.ClusterID || expected.CAFingerprint != want.CAFingerprint || expected.Protocol != want.Protocol {
		return fail(store.ErrClusterIdentityConflict)
	}
	active := false
	for _, row := range admissions {
		if row.KeyFingerprint == key && row.RevokedAt == nil {
			active = true
		}
	}
	if !active && !badResult {
		return fail(store.ErrClusterAdmissionRejected)
	}
	if !badResult {
		provider := false
		for _, member := range proof.Members {
			for _, admission := range admissions {
				if member.ID == proof.LeaderID && member.Role == "voter" && admission.KeyFingerprint == providerKey && admission.Address == member.Address && admission.RevokedAt == nil {
					provider = true
				}
			}
		}
		if !provider {
			return fail(store.ErrClusterAdmissionRejected)
		}
	}
	if err != nil {
		return fail(err)
	}
	if hold != nil {
		if err := hold(ctx); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return legacy, phase, admissions, proof, nil
}

type closedFactsTLSFixture struct {
	bundle                                            *enrollmentBundle
	session                                           *closedFactsRouteSession
	serverPair, requesterPair, otherPair, unknownPair tls.Certificate
	serverKey, requesterKey, otherKey                 string
}

func newClosedFactsTLSFixture(t *testing.T) *closedFactsTLSFixture {
	t.Helper()
	now := time.Now().UTC()
	clusterID := strings.Repeat("a", 32)
	ca, caKey, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(address string) (tls.Certificate, string) {
		t.Helper()
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
		cert, err := authority.IssueMember(csr, fp, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return pair, fp
	}
	f := &closedFactsTLSFixture{}
	f.serverPair, f.serverKey = issue("127.0.0.1:9910")
	f.requesterPair, f.requesterKey = issue("127.0.0.1:9911")
	f.otherPair, f.otherKey = issue("127.0.0.1:9912")
	f.unknownPair, _ = issue("127.0.0.1:9913")
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.serverPair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	f.bundle = &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, ListenAddress: "127.0.0.1:0", PublicURL: "https://127.0.0.1:9910"}, issuer: authority, tls: config, trust: trust, ca: ca, caKey: caKey}
	f.session = &closedFactsRouteSession{
		providerKey: f.serverKey,
		expected:    store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion},
		phase:       store.MaintenancePhaseState{Phase: store.MaintenanceFrozen, Revision: 3},
		admissions: []store.ClusterAdmission{
			{KeyFingerprint: f.serverKey, Address: "127.0.0.1:9910"},
			{KeyFingerprint: f.requesterKey, Address: "127.0.0.1:9911", InvitationHash: strings.Repeat("1", 64)},
			{KeyFingerprint: f.otherKey, Address: "127.0.0.1:9912", InvitationHash: strings.Repeat("2", 64)},
		},
		proof: cluster.QuorumProof{Term: 4, Index: 42, ConfigurationIndex: 11, LeaderID: 7, Members: []cluster.Member{
			{ID: 7, Address: "127.0.0.1:9910", Role: "voter"},
			{ID: 9, Address: "127.0.0.1:9911", Role: "voter"},
			{ID: 11, Address: "127.0.0.1:9912", Role: "voter"},
		}},
	}
	return f
}

func (f *closedFactsTLSFixture) start(t *testing.T) *enrollmentServer {
	t.Helper()
	server, err := newClosedFactsEnrollmentServer(f.bundle, f.session, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if server.run != nil || server.materialRequests == nil || server.materialReceiver != nil {
		t.Fatal("facts constructor started listener or assembled material receiver")
	}
	if f.bundle.tls.ClientAuth != tls.VerifyClientCertIfGiven || server.config.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("facts strict mTLS changed original bundle policy")
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return server
}

func (f *closedFactsTLSFixture) client(t *testing.T, pair *tls.Certificate) *http.Client {
	t.Helper()
	config, err := identity.EnrollmentTLSConfig(f.bundle.info.ClusterID, f.bundle.info.CAFingerprint, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if pair != nil {
		config.Certificates = []tls.Certificate{*pair}
	}
	transport := &http.Transport{TLSClientConfig: config, DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func closedFactsRouteRequest(t *testing.T, client *http.Client, server *enrollmentServer, method, path, body string) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+server.Addr()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Neither proxy identity nor an invitation bearer can replace actual TLS.
	req.Header.Set("X-Client-Cert", "claimed-member-certificate")
	req.Header.Set("Authorization", "Bearer ignored-public-test-token")
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response, data, err
}

const closedFactsRouteNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const closedFactsRouteBody = `{"nonce":"` + closedFactsRouteNonce + `"}`

func TestClosedFactsEnrollmentSignedTLSWire(t *testing.T) {
	for _, mode := range []string{"current", "legacy", "pending-admitted-requester"} {
		t.Run(mode, func(t *testing.T) {
			f := newClosedFactsTLSFixture(t)
			if mode == "legacy" {
				f.session.legacy = true
				f.session.phase = store.MaintenancePhaseState{}
			}
			if mode == "pending-admitted-requester" {
				f.session.proof.Members = []cluster.Member{f.session.proof.Members[0], f.session.proof.Members[2]}
			}
			server := f.start(t)
			client := f.client(t, &f.requesterPair)
			response, data, err := closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("signed facts response=%v data=%s err=%v", response, data, err)
			}
			if response.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("facts caching/content headers: %v", response.Header)
			}
			var got, want map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			members := fmt.Sprintf(`[{"id":"7","address":"127.0.0.1:9910","role":"voter","key_fingerprint":%q},{"id":"9","address":"127.0.0.1:9911","role":"voter","key_fingerprint":%q},{"id":"11","address":"127.0.0.1:9912","role":"voter","key_fingerprint":%q}]`, f.serverKey, f.requesterKey, f.otherKey)
			if mode == "pending-admitted-requester" {
				members = fmt.Sprintf(`[{"id":"7","address":"127.0.0.1:9910","role":"voter","key_fingerprint":%q},{"id":"11","address":"127.0.0.1:9912","role":"voter","key_fingerprint":%q}]`, f.serverKey, f.otherKey)
			}
			literal := fmt.Sprintf(`{"cluster_id":%q,"ca_fingerprint":%q,"protocol":%d,"nonce":%q,"legacy":%t,"phase":%q,"phase_revision":%q,"term":"4","index":"42","configuration_index":"11","leader_id":"7","members":%s}`, f.bundle.info.ClusterID, f.bundle.info.CAFingerprint, store.ClusterProtocolVersion, closedFactsRouteNonce, mode == "legacy", f.session.phase.Phase, fmt.Sprint(f.session.phase.Revision), members)
			if err := json.Unmarshal([]byte(literal), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("literal wire mismatch\ngot=%s\nwant=%s", data, literal)
			}
			facts, err := enrollment.DecodeClusterQuorumFacts(data)
			if err != nil || facts.Nonce != closedFactsRouteNonce {
				t.Fatalf("response decode: %+v %v", facts, err)
			}
			if mode == "pending-admitted-requester" {
				for _, member := range facts.Members {
					if member.KeyFingerprint == f.requesterKey {
						t.Fatal("pending admission was fabricated as a live member")
					}
				}
			}
			f.session.mu.Lock()
			seen, key, count := f.session.seenIdentity, f.session.seenRequester, f.session.calls
			f.session.mu.Unlock()
			if seen != f.session.expected || key != f.requesterKey || count != 1 {
				t.Fatalf("actual requester/expected identity mismatch: %+v %q calls=%d", seen, key, count)
			}
			// This dedicated listener has one route, no grants/materials/business.
			for _, probe := range []struct {
				method, path string
				status       int
			}{
				{http.MethodGet, enrollment.ClusterQuorumFactsPath, http.StatusMethodNotAllowed},
				{http.MethodPost, "/_cluster/members", http.StatusNotFound},
				{http.MethodPost, "/_cluster/bootstrap-secrets", http.StatusNotFound},
				{http.MethodGet, "/api/v1/nodes", http.StatusNotFound},
			} {
				r, body, err := closedFactsRouteRequest(t, client, server, probe.method, probe.path, closedFactsRouteBody)
				if err != nil || r.StatusCode != probe.status {
					t.Fatalf("route %s %s response=%v body=%s err=%v", probe.method, probe.path, r, body, err)
				}
			}
		})
	}
}

func TestClosedFactsEnrollmentRejectsTLSAndRequestIdentity(t *testing.T) {
	for _, mode := range []string{"missing-client-certificate", "wrong-client-ca", "unadmitted-client-key", "wrong-requester-san", "revoked-requester"} {
		t.Run(mode, func(t *testing.T) {
			f := newClosedFactsTLSFixture(t)
			pair := &f.requesterPair
			want := http.StatusForbidden
			wantCalls := 1
			switch mode {
			case "missing-client-certificate":
				pair, wantCalls = nil, 0
			case "wrong-client-ca":
				foreign := newClosedFactsTLSFixture(t)
				pair, wantCalls = &foreign.requesterPair, 0
			case "unadmitted-client-key":
				pair = &f.unknownPair
			case "wrong-requester-san":
				f.session.admissions[1].Address = "other.example:9911"
				f.session.proof.Members[1].Address = "other.example:9911"
			case "revoked-requester":
				now := time.Now().UTC()
				f.session.admissions[1].RevokedAt = &now
			}
			server := f.start(t)
			response, body, err := closedFactsRouteRequest(t, f.client(t, pair), server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
			if wantCalls == 0 {
				if err == nil || response != nil {
					t.Fatalf("strict TLS accepted wrong/missing certificate: response=%v body=%s err=%v", response, body, err)
				}
			} else if err != nil || response.StatusCode != want {
				t.Fatalf("requester rejection response=%v body=%s err=%v", response, body, err)
			}
			f.session.mu.Lock()
			calls := f.session.calls
			f.session.mu.Unlock()
			if calls != wantCalls {
				t.Fatalf("identity boundary reader calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}

func TestClosedFactsEnrollmentRejectsInvalidFactsAndInput(t *testing.T) {
	for _, mode := range []string{"bad-nonce", "reader-error", "expected-cluster-mismatch", "wrong-provider-key", "wrong-provider-address", "wrong-leader-tuple"} {
		t.Run(mode, func(t *testing.T) {
			f := newClosedFactsTLSFixture(t)
			body, want, wantCalls := closedFactsRouteBody, http.StatusServiceUnavailable, 1
			switch mode {
			case "bad-nonce":
				body, want, wantCalls = `{"nonce":"BAD"}`, http.StatusBadRequest, 0
			case "reader-error":
				f.session.err = errors.New("actual leader SQL reader failure")
			case "expected-cluster-mismatch":
				f.session.expected.ClusterID = strings.Repeat("b", 32)
			case "wrong-provider-key":
				f.session.badResult = true
				f.session.admissions[0].KeyFingerprint = strings.Repeat("f", 64)
			case "wrong-provider-address":
				f.session.badResult = true
				f.session.admissions[0].Address = "foreign.example:9910"
				f.session.proof.Members[0].Address = "foreign.example:9910"
			case "wrong-leader-tuple":
				f.session.badResult = true
				f.session.proof.LeaderID = 9
			}
			server := f.start(t)
			response, data, err := closedFactsRouteRequest(t, f.client(t, &f.requesterPair), server, http.MethodPost, enrollment.ClusterQuorumFactsPath, body)
			if err != nil || response.StatusCode != want {
				t.Fatalf("invalid %s response=%v data=%s err=%v", mode, response, data, err)
			}
			if _, err := enrollment.DecodeClusterQuorumFacts(data); err == nil {
				t.Fatalf("rejection published usable facts: %s", data)
			}
			f.session.mu.Lock()
			calls := f.session.calls
			seen := f.session.seenIdentity
			f.session.mu.Unlock()
			if calls != wantCalls {
				t.Fatalf("invalid input reader calls=%d want=%d", calls, wantCalls)
			}
			if mode == "expected-cluster-mismatch" && seen.ClusterID != f.bundle.info.ClusterID {
				t.Fatal("route used reader's claimed identity instead of actual bundle identity")
			}
		})
	}
}

func TestClosedFactsEnrollmentWithdrawalJoinsActualHandler(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	entered, canceled, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	f.session.hold = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		close(returned)
		return ctx.Err()
	}
	server := f.start(t)
	client := f.client(t, &f.requesterPair)
	type outcome struct {
		response *http.Response
		data     []byte
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, data, err := closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
		done <- outcome{response, data, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("signed request never reached held reader")
	}
	withdrawn := make(chan error, 1)
	withdrawCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { withdrawn <- server.materialRequests.withdraw(withdrawCtx) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("tracker withdrawal did not cancel original reader")
	}
	select {
	case err := <-withdrawn:
		t.Fatalf("withdraw returned before actual callback return: %v", err)
	default:
	}
	// Cancellation is already visible but cleanup acknowledgement remains held.
	response, body, err := closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn route admitted another read: response=%v body=%s err=%v", response, body, err)
	}
	f.session.mu.Lock()
	calls := f.session.calls
	f.session.mu.Unlock()
	if calls != 1 {
		t.Fatalf("withdrawn tracker invoked reader again: %d", calls)
	}
	releaseHandler()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("held callback did not actually return")
	}
	select {
	case err := <-withdrawn:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("withdraw did not join returned handler")
	}
	select {
	case result := <-done:
		if result.err == nil && result.response.StatusCode == http.StatusOK {
			t.Fatalf("canceled reader published HTTP200: %s", result.data)
		}
		if _, err := enrollment.DecodeClusterQuorumFacts(result.data); err == nil {
			t.Fatalf("canceled reader leaked usable facts: %s", result.data)
		}
	case <-time.After(time.Second):
		t.Fatal("signed canceled request did not finish")
	}
}

// A member whose business is open has its business routes installed on the
// facts listener. Those define no quorum route; the signed observation is
// still answered by the listener, from the same session.
func TestClosedFactsEnrollmentAnswersWhileBusinessRoutesAreInstalled(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	server := f.start(t)
	client := f.client(t, &f.requesterPair)
	business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/cluster/enrollment/certificate" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	})
	prepared, err := newEnrollmentServer(server.addr, business, server.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	prepared.materialRequests = server.materialRequests
	if err := server.adoptPreparedHandler(prepared); err != nil {
		t.Fatal(err)
	}
	response, data, err := closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("facts with business routes installed: response=%v data=%s err=%v", response, data, err)
	}
	facts, err := enrollment.DecodeClusterQuorumFacts(data)
	if err != nil || facts.Nonce != closedFactsRouteNonce || facts.LeaderID != f.session.proof.LeaderID || facts.Term != f.session.proof.Term || len(facts.Members) != len(f.session.proof.Members) {
		t.Fatalf("facts with business routes installed: %+v %v", facts, err)
	}
	response, data, err = closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumDiscoveryPath, closedFactsRouteBody)
	if err != nil || response.StatusCode == http.StatusNotFound {
		t.Fatalf("discovery with business routes installed: response=%v data=%s err=%v", response, data, err)
	}
	response, _, err = closedFactsRouteRequest(t, client, server, http.MethodPost, "/api/v1/cluster/enrollment/certificate", "{}")
	if err != nil || response.StatusCode != http.StatusAccepted {
		t.Fatalf("business route was not served by the installed routes: response=%v err=%v", response, err)
	}
	// An unauthenticated caller gets nothing more than before.
	if response, _, err := closedFactsRouteRequest(t, f.client(t, nil), server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody); err == nil {
		t.Fatalf("facts without a member certificate: %d", response.StatusCode)
	}
	// Withdrawing the listener's request owner still closes the route.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.materialRequests.withdraw(ctx); err != nil {
		t.Fatal(err)
	}
	response, data, err = closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn owner admitted a read: response=%v data=%s err=%v", response, data, err)
	}
}

// A member that does not lead says so in its answer, so the asker goes to
// the leader, and writes nothing to its log: every follower gives that
// answer. Any other failed read is logged, because the answer hides it.
func TestClosedFactsEnrollmentSaysWhyAReadFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		err          error
		body, logged string
	}{
		"not the leader":  {fmt.Errorf("read: %w", cluster.ErrQuorumReadNotLeader), "cluster quorum facts unavailable: this member is not the database leader\n", ""},
		"another failure": {errors.New("actual leader SQL reader failure"), "cluster quorum facts unavailable\n", "actual leader SQL reader failure"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newClosedFactsTLSFixture(t)
			f.session.err = tc.err
			var log bytes.Buffer
			requests := newMaterialRequestTracker()
			handler, err := closedQuorumFactsHandler(f.bundle, f.session, requests, slog.New(slog.NewTextHandler(&log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, enrollment.ClusterQuorumFactsPath, strings.NewReader(closedFactsRouteBody))
			request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.requesterPair.Leaf}}
			recorder := httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != tc.body {
				t.Fatalf("HTTP %d %q, want 503 %q", recorder.Code, recorder.Body.String(), tc.body)
			}
			if tc.logged == "" && log.Len() != 0 {
				t.Fatalf("a follower's answer was logged: %s", log.String())
			}
			if tc.logged != "" && (!strings.Contains(log.String(), "cluster quorum facts read failed") || !strings.Contains(log.String(), tc.logged)) {
				t.Fatalf("the failed read is not in the log: %q", log.String())
			}
		})
	}
}
