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
// allowed and revoked member keys where an established member keeps it, last
// changed age ago.
func writeStatusPolicy(t *testing.T, root string, allowed, revoked int, age time.Duration) {
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
	path := filepath.Join(dir, "policy.json")
	if err := identity.PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(-age)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
}

func TestStatusOverallNamesAClusterThatLostAMember(t *testing.T) {
	const ready = "Overall: ready (business reads verified)\n"
	const settled, recent = statusJoinGrace + 5*time.Minute, 5 * time.Second
	voter, spare := "voter", "spare"
	for _, c := range []struct {
		name string
		// cached is the cluster.yaml this member holds; live what the leader
		// reports, nil when no leader was observed.
		cached, live     []string
		allowed, revoked int
		// age is how long ago the transport policy last changed.
		age  time.Duration
		want string
	}{
		{"three voters", []string{voter, voter, voter}, []string{voter, voter, voter}, 3, 0, settled, ready},
		{"three voters, one of them a replacement", []string{voter, voter, voter}, []string{voter, voter, voter}, 3, 1, settled, ready},
		{"single member", []string{voter}, []string{voter}, 1, 0, settled, ready},
		{"second member of an installation", []string{voter, spare}, []string{voter, spare}, 2, 0, settled, ready},
		{"two members, policy unreadable", []string{voter, spare}, []string{voter, spare}, 0, 0, settled, ready},
		{"two members left of three, admission kept", []string{voter, voter, voter}, []string{voter, spare}, 3, 0, settled,
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n"},
		{"two members left of three, admission revoked", []string{voter, spare}, []string{voter, spare}, 2, 1, settled,
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n"},
		{"two voters left of three", []string{voter, voter}, []string{voter, voter}, 2, 1, settled,
			"Overall: degraded (business reads verified; 2 voters among 2 members, not fault tolerant)\n"},
		{"three members, one not promoted", []string{voter, voter, spare}, []string{voter, voter, spare}, 3, 0, settled,
			"Overall: degraded (business reads verified; 2 voters among 3 members, not fault tolerant)\n"},
		{"three members, one voter", []string{voter, spare, spare}, []string{voter, spare, spare}, 3, 0, settled,
			"Overall: degraded (business reads verified; 1 voter among 3 members, not fault tolerant)\n"},
		// The cache alone can trail a cluster that has three voters by now.
		{"no leader observed, cache one voter of two", []string{voter, spare}, nil, 3, 0, settled, ready},
		{"no leader observed, cache two voters of three", []string{voter, voter, spare}, nil, 3, 0, settled, ready},
		// An admission changes the policy some seconds before the admitted
		// member reaches the membership, and its vote comes later still.
		{"third member admitted, not in the membership yet", []string{voter, spare}, []string{voter, spare}, 3, 0, recent, ready},
		{"third member in the membership, not promoted yet", []string{voter, spare, spare}, []string{voter, spare, spare}, 3, 0, recent, ready},
		{"third member in the membership, one promotion left", []string{voter, voter, spare}, []string{voter, voter, spare}, 3, 0, recent, ready},
		// The same admission, long after: the join failed, or a member left
		// and kept its admission.
		{"third member admitted long ago, never joined", []string{voter, spare}, []string{voter, spare}, 3, 0, statusJoinGrace + time.Second,
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n"},
		// A revoked key says a member left, however recent the policy is.
		{"member revoked a moment ago", []string{voter, spare}, []string{voter, spare}, 2, 1, recent,
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n"},
		{"replacement joining after a revocation", []string{voter, voter, spare}, []string{voter, voter, spare}, 3, 1, recent,
			"Overall: degraded (business reads verified; 2 voters among 3 members, not fault tolerant)\n"},
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
				writeStatusPolicy(t, root, c.allowed, c.revoked, c.age)
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

// A voter whose database port refuses the connection is unreachable: the
// cluster still has three voters, but cannot lose another.
func TestStatusOverallNamesAVoterThatIsUnreachable(t *testing.T) {
	database, stop := openStatusPort(t)
	t.Cleanup(stop)
	closed, release := openStatusPort(t)
	release()
	for _, c := range []struct {
		name      string
		addresses []string // of members 8 and 9; member 7 is this one
		cached    bool     // whether cluster.yaml already lists member 9
		revoked   int
		age       time.Duration // since the transport policy last changed
		want      []string
	}{
		{"every voter answers", []string{database, database}, true, 0, statusJoinGrace + 5*time.Minute,
			[]string{"Overall: ready (business reads verified)\n", "Fault tolerant: true\n"}},
		{"one voter refuses", []string{database, closed}, true, 0, statusJoinGrace + 5*time.Minute,
			[]string{"Overall: degraded (business reads verified; 1 of 3 voters unreachable, not fault tolerant)\n", "Voters: 3\n", "Fault tolerant: false\n", "database: connection refused"}},
		{"one voter refuses, not in the cache yet", []string{database, closed}, false, 0, statusJoinGrace + 5*time.Minute,
			[]string{"Overall: degraded (business reads verified; 1 of 3 voters unreachable, not fault tolerant)\n", "Fault tolerant: false\n", "database: connection refused"}},
		{"both other voters refuse", []string{closed, closed}, true, 0, statusJoinGrace + 5*time.Minute,
			[]string{"Overall: degraded (business reads verified; 2 of 3 voters unreachable, not fault tolerant)\n", "Fault tolerant: false\n"}},
		// An installation restarts each member it has joined: the verdict
		// waits, the lines below it do not.
		{"one voter refuses while the cluster is being formed", []string{database, closed}, true, 0, time.Minute,
			[]string{"Overall: ready (business reads verified)\n", "Fault tolerant: false\n", "database: connection refused"}},
		{"one voter refuses just after a replacement was admitted", []string{database, closed}, true, 1, time.Minute,
			[]string{"Overall: degraded (business reads verified; 1 of 3 voters unreachable, not fault tolerant)\n", "Fault tolerant: false\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cached := []string{statusIdentity(7, database, 0), statusIdentity(8, c.addresses[0], 0)}
			if c.cached {
				cached = append(cached, statusIdentity(9, c.addresses[1], 0))
			}
			root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(cached...), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
			writeStatusPolicy(t, root, 3, c.revoked, c.age)
			previous, previousProof := statusQuorumFetch, statusQuorumProofRead
			t.Cleanup(func() { statusQuorumFetch, statusQuorumProofRead = previous, previousProof })
			facts := enrollment.ClusterQuorumFacts{LeaderID: 7, Term: 2, Index: 9, ConfigurationIndex: 4, Members: []enrollment.ClusterMember{
				{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: c.addresses[0], Role: "voter"}, {ID: 9, Address: c.addresses[1], Role: "voter"},
			}}
			statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				return facts, true
			}
			statusQuorumProofRead = func(context.Context, Config, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				return enrollment.ClusterQuorumFacts{}, false
			}
			report, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, statusReadyServer(t), time.Second)
			if err != nil {
				t.Fatalf("status with open business: %v\n%s", err, report)
			}
			for _, want := range c.want {
				if !strings.Contains(report, want) {
					t.Fatalf("want %q in\n%s", want, report)
				}
			}
			if strings.Count(report, "Overall: ") != 1 || !strings.Contains(report, "Business: open\n") {
				t.Fatalf("overall or business line changed\n%s", report)
			}
		})
	}
}

// What each answer of a voter's database port means for the verdict and for
// the fault tolerance line. A voter that only gave no answer within the
// timeout is weaker evidence than a refusal: the cluster is degraded either
// way, but its fault tolerance is unverified, not denied.
func TestStatusVoterAnswers(t *testing.T) {
	const ready = "Overall: ready (business reads verified)\n"
	for _, c := range []struct {
		name     string
		roles    []string // of members 8, 9, ...; member 7 is this one, a voter
		answers  []string
		overall  string
		tolerant string
	}{
		{"all open", []string{"voter", "voter"}, []string{"open", "open"}, ready, "true"},
		{"one refused", []string{"voter", "voter"}, []string{"open", "connection refused"},
			"Overall: degraded (business reads verified; 1 of 3 voters unreachable, not fault tolerant)\n", "false"},
		{"one host unreachable", []string{"voter", "voter"}, []string{"unreachable", "open"},
			"Overall: degraded (business reads verified; 1 of 3 voters unreachable, not fault tolerant)\n", "false"},
		{"one timed out", []string{"voter", "voter"}, []string{"open", "timed out"},
			"Overall: degraded (business reads verified; 1 of 3 voters did not answer within 5s, fault tolerance unverified)\n",
			"unverified (1 of 3 voters did not answer within 5s)"},
		{"one refused, one timed out", []string{"voter", "voter"}, []string{"connection refused", "timed out"},
			"Overall: degraded (business reads verified; 1 of 3 voters unreachable, 1 of 3 voters did not answer within 5s, not fault tolerant)\n", "false"},
		{"one not probed", []string{"voter", "voter"}, []string{"open", "not observed"}, ready, "true"},
		// Five voters keep a majority after losing two.
		{"one of five refused", []string{"voter", "voter", "voter", "voter"}, []string{"open", "open", "open", "connection refused"},
			"Overall: degraded (business reads verified; 1 of 5 voters unreachable)\n", "true"},
		{"one of five timed out", []string{"voter", "voter", "voter", "voter"}, []string{"open", "open", "open", "timed out"},
			"Overall: degraded (business reads verified; 1 of 5 voters did not answer within 5s)\n", "true"},
		{"two of five timed out", []string{"voter", "voter", "voter", "voter"}, []string{"open", "open", "timed out", "timed out"},
			"Overall: degraded (business reads verified; 2 of 5 voters did not answer within 5s, fault tolerance unverified)\n",
			"unverified (2 of 5 voters did not answer within 5s)"},
		// A member that is not a voter does not count, whatever it answers.
		{"a spare refused", []string{"voter", "voter", "spare"}, []string{"open", "open", "connection refused"}, ready, "true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := clusterStatus{id: 7, role: "voter", voters: 1, faultTolerant: true, business: "open", httpAddr: "127.0.0.1:1", probeTimeout: 5 * time.Second,
				members: []statusMember{{id: 7, role: "voter", source: "live raft"}}}
			for i, role := range c.roles {
				id := uint64(8 + i)
				if role == "voter" {
					s.voters++
				}
				s.members = append(s.members, statusMember{id: id, role: role, source: "live raft"})
				s.peers = append(s.peers, statusPeer{id: id, database: c.answers[i]})
			}
			report := s.format()
			if !strings.Contains(report, c.overall) || !strings.Contains(report, "Fault tolerant: "+c.tolerant+"\n") {
				t.Fatalf("want %q and fault tolerant %q in\n%s", c.overall, c.tolerant, report)
			}
		})
	}
	// The cluster.yaml cache alone names no voter as unreachable.
	s := clusterStatus{id: 7, role: "voter", voters: 3, business: "open", httpAddr: "127.0.0.1:1",
		members: []statusMember{{id: 7, role: "voter", source: "cluster.yaml cache"}, {id: 8, role: "voter", source: "cluster.yaml cache"}, {id: 9, role: "voter", source: "cluster.yaml cache"}},
		peers:   []statusPeer{{id: 8, database: "connection refused"}, {id: 9, database: "open"}}}
	if report := s.format(); !strings.Contains(report, ready) {
		t.Fatalf("a cached voter was read as unreachable\n%s", report)
	}
}

func TestStatusShortfallDoesNotReplaceUnverifiedOrNotReady(t *testing.T) {
	s := clusterStatus{voters: 1, enrolledKeys: 3, business: "not observed", term: "2", members: []statusMember{
		{id: 7, role: "voter", voter: true, source: "live raft"}, {id: 8, role: "spare", source: "live raft"},
	}}
	if s.membershipShortfall(time.Now()) == "" {
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
