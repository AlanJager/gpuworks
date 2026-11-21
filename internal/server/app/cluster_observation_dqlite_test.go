//go:build linux && dqlite

package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// The observation a module reads and the report an operator reads are taken
// from the same member directory in each case: every verdict of one has to be
// the verdict of the other.
func TestHostClusterObservationAgreesWithTheStatusReport(t *testing.T) {
	database, stop := openStatusPort(t)
	t.Cleanup(stop)
	other, stopOther := openStatusPort(t)
	t.Cleanup(stopOther)
	closed, release := openStatusPort(t)
	release()
	old := statusJoinGrace + 5*time.Minute
	for _, c := range []struct {
		name      string
		members   []enrollment.ClusterMember // as the leader reports them; member 7 is this one
		keys      int
		revoked   int
		age       time.Duration
		tolerance ClusterFaultTolerance
		forming   bool
		shortfall ClusterShortfall
		overall   string
		tolerant  string
		databases map[uint64]string
	}{
		{"three voters answer",
			[]enrollment.ClusterMember{{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: other, Role: "voter"}, {ID: 9, Address: other, Role: "voter"}},
			3, 0, old, ClusterTolerant, false, ClusterShortfall{},
			"Overall: ready (business reads verified)\n", "Fault tolerant: true\n",
			map[uint64]string{7: "open", 8: "open", 9: "open"}},
		{"one voter refuses",
			[]enrollment.ClusterMember{{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: other, Role: "voter"}, {ID: 9, Address: closed, Role: "voter"}},
			3, 0, old, ClusterNotTolerant, false, ClusterShortfall{Kind: ClusterShortfallVotersMissing, Voters: 3, Members: 3, Unreachable: 1},
			"Overall: degraded (business reads verified; 1 of 3 voters unreachable, not fault tolerant)\n", "Fault tolerant: false\n",
			map[uint64]string{7: "open", 8: "open", 9: "connection refused"}},
		{"one voter refuses while the cluster is being formed",
			[]enrollment.ClusterMember{{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: other, Role: "voter"}, {ID: 9, Address: closed, Role: "voter"}},
			3, 0, time.Minute, ClusterNotTolerant, true, ClusterShortfall{},
			"Overall: ready (business reads verified)\n", "Fault tolerant: false\n",
			map[uint64]string{7: "open", 8: "open", 9: "connection refused"}},
		{"a member left: one voter and one spare",
			[]enrollment.ClusterMember{{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: other, Role: "spare"}},
			3, 0, old, ClusterNotTolerant, false, ClusterShortfall{Kind: ClusterShortfallFewVoters, Voters: 1, Members: 2},
			"Overall: degraded (business reads verified; 1 voter among 2 members, not fault tolerant)\n", "Fault tolerant: false\n",
			map[uint64]string{7: "open", 8: "open"}},
		{"an installation between its second and third member",
			[]enrollment.ClusterMember{{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: other, Role: "voter"}},
			2, 0, old, ClusterNotTolerant, false, ClusterShortfall{},
			"Overall: ready (business reads verified)\n", "Fault tolerant: false\n",
			map[uint64]string{7: "open", 8: "open"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var cached []string
			for _, member := range c.members {
				role := 0
				if member.Role == "spare" {
					role = 2
				}
				cached = append(cached, statusIdentity(int(member.ID), member.Address, role))
			}
			root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(cached...), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
			writeStatusEnrollment(t, root, database)
			writeStatusPolicy(t, root, c.keys-c.revoked, c.revoked, c.age)
			previous, previousProof := statusQuorumFetch, statusQuorumProofRead
			t.Cleanup(func() { statusQuorumFetch, statusQuorumProofRead = previous, previousProof })
			facts := enrollment.ClusterQuorumFacts{LeaderID: 8, Term: 2, Index: 9, ConfigurationIndex: 4, Members: c.members}
			statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				return facts, true
			}
			statusQuorumProofRead = func(context.Context, Config, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				return enrollment.ClusterQuorumFacts{}, false
			}
			cfg := Config{DataDir: root}
			before := time.Now()
			got, err := (&Host{name: "probe", cfg: cfg}).ClusterObservation(context.Background(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Clustered || got.ObservedAt.Before(before) || got.ProbeTimeout != time.Second || got.LeaderID != 8 {
				t.Fatalf("observation header = %+v", got)
			}
			if got.FaultTolerance != c.tolerance || got.Forming != c.forming || got.Shortfall != c.shortfall {
				t.Fatalf("verdicts = %q forming=%v %+v, want %q forming=%v %+v", got.FaultTolerance, got.Forming, got.Shortfall, c.tolerance, c.forming, c.shortfall)
			}
			databases := map[uint64]string{}
			selves := 0
			for i, member := range got.Members {
				databases[member.ID] = member.Database
				if member.Self {
					selves++
					if member.ID != 7 || member.Listener != "open" {
						t.Fatalf("self = %+v", member)
					}
				}
				if !member.Live || (i > 0 && got.Members[i-1].Address > member.Address) {
					t.Fatalf("members not live or not ordered by address: %+v", got.Members)
				}
			}
			if selves != 1 || !reflect.DeepEqual(databases, c.databases) {
				t.Fatalf("members = %+v", got.Members)
			}
			report, err := ObserveClusterStatus(context.Background(), cfg, statusReadyServer(t), time.Second)
			if err != nil {
				t.Fatalf("status with open business: %v\n%s", err, report)
			}
			if !strings.Contains(report, c.overall) || !strings.Contains(report, c.tolerant) {
				t.Fatalf("the report disagrees: want %q and %q in\n%s", c.overall, c.tolerant, report)
			}
		})
	}
}

// A voter that only gave no answer in time leaves fault tolerance open in
// both readings.
func TestClusterObservationVerdictFollowsTheToleranceLine(t *testing.T) {
	s := clusterStatus{id: 7, voters: 3, faultTolerant: true, probeTimeout: 2 * time.Second,
		members: []statusMember{{id: 7, role: "voter", source: "live raft"}, {id: 8, role: "voter", source: "live raft"}, {id: 9, role: "voter", source: "live raft"}},
		peers:   []statusPeer{{id: 8, database: "open"}, {id: 9, database: "timed out"}}}
	if got := s.faultToleranceVerdict(); got != ClusterToleranceUnverified {
		t.Fatalf("verdict = %q", got)
	}
	if got := s.faultTolerance(); got != "unverified (1 of 3 voters did not answer within 2s)" {
		t.Fatalf("line = %q", got)
	}
	short := s.shortfall(time.Now())
	if short != (ClusterShortfall{Kind: ClusterShortfallVotersMissing, Voters: 3, Members: 3, Silent: 1}) {
		t.Fatalf("shortfall = %+v", short)
	}
	if got := s.membershipShortfall(time.Now()); got != "1 of 3 voters did not answer within 2s, fault tolerance unverified" {
		t.Fatalf("wording = %q", got)
	}
}
