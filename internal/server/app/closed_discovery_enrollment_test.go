//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// This reader borrows the signed facts fixture's actual identities/admissions.
// It supports follower hints independently of that fixture's leader proof.
type closedDiscoveryRouteSession struct {
	*closedFactsRouteSession
	members        []store.ClusterTransportMember
	discoveryCalls int
}

func newClosedDiscoveryRouteFixture(t *testing.T) (*closedFactsTLSFixture, *closedDiscoveryRouteSession) {
	t.Helper()
	f := newClosedFactsTLSFixture(t)
	s := &closedDiscoveryRouteSession{closedFactsRouteSession: f.session}
	for _, member := range f.session.proof.Members {
		for _, admission := range f.session.admissions {
			if admission.Address == member.Address {
				s.members = append(s.members, store.ClusterTransportMember{ID: member.ID, Address: member.Address, Role: member.Role, KeyFingerprint: admission.KeyFingerprint, EnrollmentOrigin: admission.EnrollmentOrigin})
			}
		}
	}
	return f, s
}

func (s *closedDiscoveryRouteSession) ReadQuorumDiscoveryForCluster(ctx context.Context, expected store.ClusterIdentity, key string) (store.ClusterAdmission, []store.ClusterTransportMember, error) {
	s.mu.Lock()
	s.discoveryCalls++
	s.seenIdentity, s.seenRequester = expected, key
	want, failure, hold := s.expected, s.err, s.hold
	admissions := append([]store.ClusterAdmission(nil), s.admissions...)
	members := append([]store.ClusterTransportMember(nil), s.members...)
	s.mu.Unlock()
	fail := func(err error) (store.ClusterAdmission, []store.ClusterTransportMember, error) {
		return store.ClusterAdmission{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if want.ClusterID != expected.ClusterID || want.CAFingerprint != expected.CAFingerprint || want.Protocol != expected.Protocol {
		return fail(store.ErrClusterIdentityConflict)
	}
	var requester store.ClusterAdmission
	for _, admission := range admissions {
		if admission.KeyFingerprint == key && admission.RevokedAt == nil {
			requester = admission
		}
	}
	if requester.KeyFingerprint == "" {
		return fail(store.ErrClusterAdmissionRejected)
	}
	if failure != nil {
		return fail(failure)
	}
	if hold != nil {
		if err := hold(ctx); err != nil {
			return fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return requester, members, nil
}

func startClosedDiscoveryFixture(t *testing.T, f *closedFactsTLSFixture, s *closedDiscoveryRouteSession) *enrollmentServer {
	t.Helper()
	server, err := newClosedFactsEnrollmentServer(f.bundle, s, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if server.run != nil || server.materialReceiver != nil || server.materialRequests == nil {
		t.Fatal("discovery constructor started listener or material services")
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

func TestClosedDiscoveryEnrollmentFollowerSignedTLSWire(t *testing.T) {
	for _, mode := range []string{"follower", "pending-admitted-requester"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newClosedDiscoveryRouteFixture(t)
			s.proof.LeaderID = 11 // The actual listener remains member 7, not the leader.
			s.members[0].EnrollmentOrigin = "https://127.0.0.1:9980"
			if mode == "pending-admitted-requester" {
				s.members = []store.ClusterTransportMember{s.members[2], s.members[0]}
			} else {
				s.members[0], s.members[2] = s.members[2], s.members[0]
			}
			before := append([]store.ClusterAdmission(nil), s.admissions...)
			server := startClosedDiscoveryFixture(t, f, s)
			response, data, err := closedFactsRouteRequest(t, f.client(t, &f.requesterPair), server, http.MethodPost, enrollment.ClusterQuorumDiscoveryPath, closedFactsRouteBody)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("response=%v body=%s err=%v", response, data, err)
			}
			if response.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("headers=%v", response.Header)
			}
			members := fmt.Sprintf(`[{"id":"7","address":"127.0.0.1:9910","role":"voter","key_fingerprint":%q,"enrollment_origin":"https://127.0.0.1:9980"},{"id":"9","address":"127.0.0.1:9911","role":"voter","key_fingerprint":%q},{"id":"11","address":"127.0.0.1:9912","role":"voter","key_fingerprint":%q}]`, f.serverKey, f.requesterKey, f.otherKey)
			if mode == "pending-admitted-requester" {
				members = fmt.Sprintf(`[{"id":"7","address":"127.0.0.1:9910","role":"voter","key_fingerprint":%q,"enrollment_origin":"https://127.0.0.1:9980"},{"id":"11","address":"127.0.0.1:9912","role":"voter","key_fingerprint":%q}]`, f.serverKey, f.otherKey)
			}
			literal := fmt.Sprintf(`{"cluster_id":%q,"ca_fingerprint":%q,"protocol":1,"nonce":%q,"members":%s}`, f.bundle.info.ClusterID, f.bundle.info.CAFingerprint, closedFactsRouteNonce, members)
			var got, want map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(literal), &want); err != nil {
				t.Fatal(err)
			}
			if len(got) != 5 || !reflect.DeepEqual(got, want) {
				t.Fatalf("dial hints wire mismatch\ngot=%s\nwant=%s", data, literal)
			}
			if _, err := enrollment.DecodeClusterQuorumDiscovery(data); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			calls, factsCalls, seen, seenKey := s.discoveryCalls, s.calls, s.seenIdentity, s.seenRequester
			s.mu.Unlock()
			if calls != 1 || factsCalls != 0 || seen != s.expected || seenKey != f.requesterKey || !reflect.DeepEqual(before, s.admissions) {
				t.Fatal("discovery used quorum reader, changed admissions, or substituted identity")
			}
		})
	}
}

func TestClosedDiscoveryEnrollmentTLSAndAdmissionRejection(t *testing.T) {
	for _, mode := range []string{"missing-client-certificate", "wrong-client-ca", "unadmitted-client-key", "wrong-requester-san", "revoked-requester"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newClosedDiscoveryRouteFixture(t)
			pair, wantCalls := &f.requesterPair, 1
			switch mode {
			case "missing-client-certificate":
				pair, wantCalls = nil, 0
			case "wrong-client-ca":
				foreign := newClosedFactsTLSFixture(t)
				pair, wantCalls = &foreign.requesterPair, 0
			case "unadmitted-client-key":
				pair = &f.unknownPair
			case "wrong-requester-san":
				s.admissions[1].Address = "other.example:9911"
			case "revoked-requester":
				now := time.Now().UTC()
				s.admissions[1].RevokedAt = &now
			}
			server := startClosedDiscoveryFixture(t, f, s)
			response, data, err := closedFactsRouteRequest(t, f.client(t, pair), server, http.MethodPost, enrollment.ClusterQuorumDiscoveryPath, closedFactsRouteBody)
			if wantCalls == 0 {
				if err == nil || response != nil {
					t.Fatalf("invalid TLS accepted: response=%v body=%s err=%v", response, data, err)
				}
			} else if err != nil || response.StatusCode != http.StatusForbidden {
				t.Fatalf("response=%v body=%s err=%v", response, data, err)
			}
			s.mu.Lock()
			calls := s.discoveryCalls
			s.mu.Unlock()
			if calls != wantCalls {
				t.Fatalf("reader calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}

func TestClosedDiscoveryEnrollmentRejectsInvalidHintsAndInput(t *testing.T) {
	for _, mode := range []string{"bad-nonce", "oversized-request", "reader-error", "expected-cluster-mismatch", "wrong-provider-key", "wrong-provider-san", "duplicate-provider-key", "duplicate-id", "invalid-role", "invalid-origin", "empty-members", "too-many-members"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newClosedDiscoveryRouteFixture(t)
			body, want, wantCalls := closedFactsRouteBody, http.StatusServiceUnavailable, 1
			switch mode {
			case "bad-nonce":
				body, want, wantCalls = `{"nonce":"BAD"}`, http.StatusBadRequest, 0
			case "oversized-request":
				body, want, wantCalls = strings.Repeat(" ", 4097)+closedFactsRouteBody, http.StatusBadRequest, 0
			case "reader-error":
				s.err = errors.New("native discovery SQL failed")
			case "expected-cluster-mismatch":
				s.expected.ClusterID = strings.Repeat("b", 32)
			case "wrong-provider-key":
				s.members[0].KeyFingerprint = strings.Repeat("f", 64)
			case "wrong-provider-san":
				s.members[0].Address = "foreign.example:9910"
			case "duplicate-provider-key":
				s.members[1].KeyFingerprint = f.serverKey
			case "duplicate-id":
				s.members[1].ID = s.members[0].ID
			case "invalid-role":
				s.members[1].Role = "leader"
			case "invalid-origin":
				s.members[1].EnrollmentOrigin = "http://127.0.0.1:9981"
			case "empty-members":
				s.members = nil
			case "too-many-members":
				s.members = make([]store.ClusterTransportMember, 513)
			}
			server := startClosedDiscoveryFixture(t, f, s)
			response, data, err := closedFactsRouteRequest(t, f.client(t, &f.requesterPair), server, http.MethodPost, enrollment.ClusterQuorumDiscoveryPath, body)
			if err != nil || response.StatusCode != want {
				t.Fatalf("response=%v body=%s err=%v", response, data, err)
			}
			if _, err := enrollment.DecodeClusterQuorumDiscovery(data); err == nil {
				t.Fatalf("rejected request published hints: %s", data)
			}
			s.mu.Lock()
			calls, seen := s.discoveryCalls, s.seenIdentity
			s.mu.Unlock()
			if calls != wantCalls {
				t.Fatalf("reader calls=%d want=%d", calls, wantCalls)
			}
			if mode == "expected-cluster-mismatch" && seen.ClusterID != f.bundle.info.ClusterID {
				t.Fatal("route trusted reader identity")
			}
		})
	}
}

func TestClosedDiscoveryEnrollmentUnsupportedAndRouteInventory(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	server := f.start(t) // Existing facts-only sessions do not gain fake discovery.
	client := f.client(t, &f.requesterPair)
	for _, row := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, enrollment.ClusterQuorumDiscoveryPath, http.StatusServiceUnavailable},
		{http.MethodGet, enrollment.ClusterQuorumDiscoveryPath, http.StatusMethodNotAllowed},
		{http.MethodPost, "/_cluster/members", http.StatusNotFound},
		{http.MethodPost, "/_cluster/material", http.StatusNotFound},
		{http.MethodPost, "/api/v1/cluster/enrollment/certificate", http.StatusNotFound},
	} {
		response, data, err := closedFactsRouteRequest(t, client, server, row.method, row.path, closedFactsRouteBody)
		if err != nil || response.StatusCode != row.status {
			t.Fatalf("%s %s response=%v body=%s err=%v", row.method, row.path, response, data, err)
		}
	}
	if f.session.calls != 0 {
		t.Fatal("unsupported discovery called quorum/grant reader")
	}
}

func TestClosedDiscoveryEnrollmentWithdrawalJoinsActualHandler(t *testing.T) {
	f, s := newClosedDiscoveryRouteFixture(t)
	entered, canceled, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseHandler := func() { once.Do(func() { close(release) }) }
	defer releaseHandler()
	s.hold = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		close(returned)
		return ctx.Err()
	}
	server := startClosedDiscoveryFixture(t, f, s)
	client := f.client(t, &f.requesterPair)
	type outcome struct {
		response *http.Response
		data     []byte
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, data, err := closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumDiscoveryPath, closedFactsRouteBody)
		done <- outcome{response, data, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request never reached discovery reader")
	}
	withdrawn := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { withdrawn <- server.materialRequests.withdraw(ctx) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("tracker did not cancel actual reader")
	}
	select {
	case err := <-withdrawn:
		t.Fatalf("withdraw returned before callback: %v", err)
	default:
	}
	response, data, err := closedFactsRouteRequest(t, client, server, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("same owner admitted facts during withdrawal: response=%v body=%s err=%v", response, data, err)
	}
	s.mu.Lock()
	discoveryCalls, factsCalls := s.discoveryCalls, s.calls
	s.mu.Unlock()
	if discoveryCalls != 1 || factsCalls != 0 {
		t.Fatal("withdrawal failed to close both read routes")
	}
	releaseHandler()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("held callback never returned")
	}
	select {
	case err := <-withdrawn:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("withdraw did not join callback")
	}
	select {
	case result := <-done:
		if result.err == nil && result.response.StatusCode == http.StatusOK {
			t.Fatalf("canceled reader emitted implicit 200: %s", result.data)
		}
		if _, err := enrollment.DecodeClusterQuorumDiscovery(result.data); err == nil {
			t.Fatal("canceled read leaked usable hints")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled signed request did not finish")
	}
}
