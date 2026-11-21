//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

func TestStatusAppliesLiveQuorumWithoutTreatingIndexAsMaterialReadiness(t *testing.T) {
	self := closedStatusPort(t)
	database := closedStatusPort(t)
	origin := closedStatusPort(t)
	root := writeStatusRaft(t, statusIdentity(7, self, 0), statusMembers(
		statusIdentity(7, self, 2),
		statusIdentity(8, database, 0),
	), map[string]string{"dqlite-lock": ""})
	previous := statusQuorumFetch
	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{
			LeaderID: 8, Term: 4, Index: 20, ConfigurationIndex: 8,
			Members: []enrollment.ClusterMember{
				{ID: 8, Address: database, Role: "voter", EnrollmentOrigin: "https://" + origin},
				{ID: 7, Address: self, Role: "spare"},
			},
		}, true
	}
	t.Cleanup(func() { statusQuorumFetch = previous })

	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"Role: spare (live raft)",
		"Voters: 1",
		"Fault tolerant: false",
		"Sync: not replicating",
		"persisted raft directory has only identity files",
		"materials_ready: not observed",
		"Leader: 8 " + database,
		"Term: 4",
		"Commit index: 20",
		"Configuration index: 8",
		"  7 " + self + " spare (live raft)",
		"  8 " + database + " voter (live raft)",
		"database: connection refused enrollment: refused material route: not observed",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q\n%s", line, text)
		}
	}
	if strings.Contains(text, "(self)") || strings.Contains(text, "Commit index: 0") || strings.Contains(text, "Seconds_Behind") || strings.Contains(text, "Fault tolerant: true") || strings.Contains(text, "unreachable") {
		t.Fatalf("live quorum invented leadership, lag, or an unreachable peer\n%s", text)
	}
}

func TestStatusLiveQuorumMarksThreeVotersFaultTolerant(t *testing.T) {
	// The other two voters answer: one that does not is not fault tolerance.
	peer, stop := openStatusPort(t)
	t.Cleanup(stop)
	root := writeStatusRaft(t, statusIdentity(1, "127.0.0.1:1", 0), statusMembers(
		statusIdentity(1, "127.0.0.1:1", 0),
		statusIdentity(2, peer, 0),
		statusIdentity(3, peer, 0),
	), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
	previous := statusQuorumFetch
	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{
			LeaderID: 1, Term: 2, Index: 9, ConfigurationIndex: 4,
			Members: []enrollment.ClusterMember{
				{ID: 1, Address: "127.0.0.1:1", Role: "voter"},
				{ID: 2, Address: peer, Role: "voter"},
				{ID: 3, Address: peer, Role: "voter"},
			},
		}, true
	}
	t.Cleanup(func() { statusQuorumFetch = previous })
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"Role: voter (live raft)", "Voters: 3", "Fault tolerant: true", "Sync: observed", "Leader: 1 127.0.0.1:1 (self)", "materials_ready: not observed"} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q\n%s", line, text)
		}
	}
}

func TestStatusMaterialProbeKeepsTheUnregisteredPageApartFromARegisteredMiss(t *testing.T) {
	if got := classifyMaterialProbe(http.StatusNotFound, []byte("404 page not found\n")); got != "HTTP 404 404 page not found" {
		t.Fatal(got)
	}
	if got := classifyMaterialProbe(http.StatusNotFound, []byte("material unavailable\n")); got != "registered" {
		t.Fatal(got)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	response, err := http.Get(server.URL + materialReplicaPath + "/" + strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := make([]byte, 128)
	n, _ := response.Body.Read(body)
	if got := classifyMaterialProbe(response.StatusCode, body[:n]); got != "HTTP 404 404 page not found" {
		t.Fatalf("unregistered route classified as %q", got)
	}
}
