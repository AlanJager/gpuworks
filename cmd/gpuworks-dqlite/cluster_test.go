//go:build linux && cgo && dqlite

package main

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// Three helpers on loopback with real cluster identity: the seed and two
// joiners form a voting cluster, a write on one is read through another, and
// the two survivors elect a leader after the first one is killed.
func TestThreeMembersJoinReplicateAndSurviveLeaderLoss(t *testing.T) {
	threeMembersSurviveLeaderLoss(t, nil, 0)
}

// On the slow-storage tier every member is started with the tier's latency. A
// follower then waits at least one election timeout (750 ms) before it
// campaigns, where the standard tier's followers wait 150 to 300 ms.
func TestThreeMembersOnTheSlowStorageTierElectOnItsTimer(t *testing.T) {
	latency := strconv.FormatInt(storagetier.SlowStorage.NetworkLatency().Milliseconds(), 10)
	threeMembersSurviveLeaderLoss(t, []string{"--network-latency-ms", latency}, storagetier.SlowStorage.ElectionTimeout())
}

// threeMembersSurviveLeaderLoss runs the join, replication and leader-loss
// scenario with extra arguments on every member. A positive earliest is the
// least time the survivors may take to name a new leader.
func threeMembersSurviveLeaderLoss(t *testing.T, extra []string, earliest time.Duration) {
	cluster := newTestCluster(t, "00112233445566778899aabbccddeeff")
	members := []testMember{
		cluster.member("a", freeAddress(t)),
		cluster.member("b", freeAddress(t)),
		cluster.member("c", freeAddress(t)),
	}
	cluster.publish(members, nil)
	nodes := make([]*served, len(members))
	nodes[0] = startServe(t, cluster.serveArgs(dataDir(t), members[0], extra...)...)
	for i := 1; i < len(members); i++ {
		nodes[i] = startServe(t, cluster.serveArgs(dataDir(t), members[i], append([]string{"--seeds", members[0].address}, extra...)...)...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// leaderVia asks one member who leads, retrying while an election runs.
	leaderVia := func(via int, exclude string) string {
		t.Helper()
		for {
			attempt, stop := context.WithTimeout(ctx, 2*time.Second)
			conn, err := client.New(attempt, members[via].address, client.WithDialFunc(cluster.dial(members[via])))
			if err == nil {
				leader, leaderErr := conn.Leader(attempt)
				_ = conn.Close()
				if leaderErr == nil && leader != nil && leader.Address != "" && leader.Address != exclude {
					stop()
					return leader.Address
				}
			}
			stop()
			select {
			case <-ctx.Done():
				t.Fatalf("no leader seen through %s", members[via].address)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	voters := func(via int, leader string) int {
		t.Helper()
		for i, m := range members {
			if m.address != leader {
				continue
			}
			conn, err := client.New(ctx, leader, client.WithDialFunc(cluster.dial(members[i])))
			if err != nil {
				return 0
			}
			defer conn.Close()
			all, err := conn.Cluster(ctx)
			if err != nil {
				return 0
			}
			count := 0
			for _, member := range all {
				if member.Role == client.Voter {
					count++
				}
			}
			return count
		}
		return 0
	}
	first := leaderVia(0, "")
	for voters(0, leaderVia(0, "")) != 3 {
		select {
		case <-ctx.Done():
			t.Fatal("the joiners never became voters")
		case <-time.After(200 * time.Millisecond):
		}
	}
	first = leaderVia(1, "")

	writer := openSQL(t, members[0].address, cluster.dial(members[0]))
	if _, err := writer.ExecContext(ctx, "CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO settings VALUES('sentinel','before')"); err != nil {
		t.Fatal(err)
	}
	reader := openSQL(t, members[2].address, cluster.dial(members[2]))
	var value string
	if err := reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'sentinel'").Scan(&value); err != nil || value != "before" {
		t.Fatalf("read through another member: %q, %v", value, err)
	}
	_ = writer.Close()
	_ = reader.Close()

	lost := -1
	for i, m := range members {
		if m.address == first {
			lost = i
		}
	}
	if lost < 0 {
		t.Fatalf("leader %q is not a member", first)
	}
	if err := nodes[lost].cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = nodes[lost].cmd.Process.Wait()
	killed := time.Now()
	survivor := (lost + 1) % len(members)
	next := leaderVia(survivor, first)
	if next == first {
		t.Fatal("the killed member still leads")
	}
	if elapsed := time.Since(killed); earliest > 0 && elapsed < earliest {
		t.Fatalf("a new leader was named %s after the old one was killed; followers on this tier wait at least %s", elapsed, earliest)
	}
	after := openSQL(t, members[survivor].address, cluster.dial(members[survivor]))
	if _, err := after.ExecContext(ctx, "INSERT INTO settings VALUES('later','after')"); err != nil {
		t.Fatalf("write after the leader was lost: %v", err)
	}
	var count int
	if err := after.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE (key = 'sentinel' AND value = 'before') OR key = 'later'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows after the leader was lost: %d, %v", count, err)
	}
	_ = after.Close()
	for i, node := range nodes {
		if i != lost {
			node.stop(t, syscall.SIGTERM)
		}
	}
}
