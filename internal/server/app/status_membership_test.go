//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

func statusReadyServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String()
}

// writeStatusPolicy publishes a transport policy with the given numbers of
// allowed and revoked member keys where an established member keeps it.
func writeStatusPolicy(t *testing.T, root string, allowed, revoked int) {
	t.Helper()
	dir := filepath.Join(root, "gpuworks.db.dqlite", ".transport")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := identity.TransportPolicy{ClusterID: strings.Repeat("a", 32), CAFingerprint: strings.Repeat("b", 64), Revision: 1}
	for i := range allowed + revoked {
		key := strings.Repeat(string(rune('0'+i)), 64)
		if i < allowed {
			policy.AllowedKeys = append(policy.AllowedKeys, key)
		} else {
			policy.RevokedKeys = append(policy.RevokedKeys, key)
		}
	}
	if err := identity.PublishTransportPolicy(filepath.Join(dir, "policy.json"), 0, policy); err != nil {
		t.Fatal(err)
	}
}

func TestStatusOverallNamesAClusterThatLostAMember(t *testing.T) {
	const ready = "Overall: ready (business reads verified)\n"
	voter, spare := "voter", "spare"
	for _, c := range []struct {
		name string
		// cached is the cluster.yaml this member holds; live what the leader
		// reports, nil when no leader was observed.
		cached, live     []string
		allowed, revoked int
		want             string
	}{
		{"three voters", []string{voter, voter, voter}, []string{voter, voter, voter}, 3, 0, ready},
		{"three voters, one of them a replacement", []string{voter, voter, voter}, []string{voter, voter, voter}, 3, 1, ready},
		{"single member", []string{voter}, []string{voter}, 1, 0, ready},
		{"second member of an installation", []string{voter, spare}, []string{voter, spare}, 2, 0, ready},
		{"two members, policy unreadable", []string{voter, spare}, []string{voter, spare}, 0, 0, ready},
		{"two members left of three, admission kept", []string{voter, voter, voter}, []string{voter, spare}, 3, 0,
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n"},
		{"two members left of three, admission revoked", []string{voter, spare}, []string{voter, spare}, 2, 1,
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n"},
		{"two voters left of three", []string{voter, voter}, []string{voter, voter}, 2, 1,
			"Overall: degraded (business reads verified; 2 voters among 2 members, not fault tolerant)\n"},
		{"three members, one not promoted", []string{voter, voter, spare}, []string{voter, voter, spare}, 3, 0,
			"Overall: degraded (business reads verified; 2 voters among 3 members, not fault tolerant)\n"},
		{"three members, one voter", []string{voter, spare, spare}, []string{voter, spare, spare}, 3, 0,
			"Overall: degraded (business reads verified; 1 voter among 3 members, not fault tolerant)\n"},
		// The cache alone can trail a cluster that has three voters by now.
		{"no leader observed, cache one voter of two", []string{voter, spare}, nil, 3, 0, ready},
		{"no leader observed, cache two voters of three", []string{voter, voter, spare}, nil, 3, 0, ready},
	} {
		t.Run(c.name, func(t *testing.T) {
			database, stop := openStatusPort(t)
			t.Cleanup(stop)
			role := map[string]int{voter: 0, spare: 2}
			var cached []string
			for i, r := range c.cached {
				cached = append(cached, statusIdentity(7+i, database, role[r]))
			}
			root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(cached...), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
			if c.allowed+c.revoked > 0 {
				writeStatusPolicy(t, root, c.allowed, c.revoked)
			}
			previous, previousProof := statusQuorumFetch, statusQuorumProofRead
			t.Cleanup(func() { statusQuorumFetch, statusQuorumProofRead = previous, previousProof })
			facts := enrollment.ClusterQuorumFacts{LeaderID: 7, Term: 2, Index: 9, ConfigurationIndex: 4}
			for i, r := range c.live {
				facts.Members = append(facts.Members, enrollment.ClusterMember{ID: uint64(7 + i), Address: database, Role: r})
			}
			statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				return facts, c.live != nil
			}
			statusQuorumProofRead = func(context.Context, Config, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				return enrollment.ClusterQuorumFacts{}, false
			}
			// Business answers in every case: the report's error and exit
			// status follow business alone.
			report, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, statusReadyServer(t), time.Second)
			if err != nil {
				t.Fatalf("status with open business: %v\n%s", err, report)
			}
			if !strings.Contains(report, c.want) || strings.Count(report, "Overall: ") != 1 {
				t.Fatalf("want %q in\n%s", c.want, report)
			}
			if !strings.Contains(report, "Business: open\n") {
				t.Fatalf("business line changed\n%s", report)
			}
		})
	}
}

func TestStatusShortfallDoesNotReplaceUnverifiedOrNotReady(t *testing.T) {
	s := clusterStatus{voters: 1, enrolledKeys: 3, business: "not observed", term: "2", members: []statusMember{
		{id: 7, role: "voter", voter: true, source: "live raft"}, {id: 8, role: "spare", source: "live raft"},
	}}
	if s.membershipShortfall() == "" {
		t.Fatal("fixture is not short of members")
	}
	if report := s.format(); !strings.Contains(report, "Overall: unverified (supply --http-addr to check business readiness)\n") {
		t.Fatalf("unverified wording changed\n%s", report)
	}
	s.httpAddr, s.business = "127.0.0.1:1", "member business service is not ready"
	if report := s.format(); !strings.Contains(report, "Overall: not ready (business: member business service is not ready)\n") {
		t.Fatalf("not ready wording changed\n%s", report)
	}
	// A member the cache still lists but the leader no longer reports is not
	// counted among the members.
	s.business = "open"
	s.members = append(s.members, statusMember{id: 9, role: "voter", voter: true, source: "cluster.yaml cache"})
	if report := s.format(); !strings.Contains(report, "Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n") {
		t.Fatalf("a cached member was counted\n%s", report)
	}
}
