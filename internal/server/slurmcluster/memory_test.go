package slurmcluster

import (
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// atClock is a memory whose clock the test moves.
func atClock(t *testing.T) (*Memory, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	m := New()
	m.SetClock(func() time.Time { return now })
	return m, &now
}

func TestSetupCodeServesOnce(t *testing.T) {
	m, _ := atClock(t)
	code, err := m.IssueSetupCode(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) < 32 {
		t.Fatalf("code %q is too short to be a secret", code)
	}
	if id, ok := m.TakeSetupCode(code); !ok || id != 7 {
		t.Fatalf("first take = %d, %v", id, ok)
	}
	for _, again := range []string{code, "not-a-code", ""} {
		if _, ok := m.TakeSetupCode(again); ok {
			t.Fatalf("code %q was accepted", again)
		}
	}
}

func TestSetupCodeExpiresAndExpiredOnesAreSwept(t *testing.T) {
	m, now := atClock(t)
	late, _ := m.IssueSetupCode(1)
	edge, _ := m.IssueSetupCode(2)
	fresh, _ := m.IssueSetupCode(3)
	*now = now.Add(OnceTTL - time.Second)
	if id, ok := m.TakeSetupCode(fresh); !ok || id != 3 {
		t.Fatalf("a code just inside its lifetime = %d, %v", id, ok)
	}
	*now = now.Add(time.Second)
	if _, ok := m.TakeSetupCode(edge); ok {
		t.Fatal("a code was accepted at the end of its lifetime")
	}
	*now = now.Add(time.Hour)
	if _, ok := m.TakeSetupCode(late); ok {
		t.Fatal("an expired code was accepted")
	}
	dead, _ := m.IssueSetupCode(4)
	*now = now.Add(2 * OnceTTL)
	if _, err := m.IssueSetupCode(5); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, kept := m.once[dead]
	n := len(m.once)
	m.mu.Unlock()
	if kept || n != 1 {
		t.Fatalf("expired codes kept: %d", n)
	}
}

func TestSetupCodeConcurrentTakesHaveOneWinner(t *testing.T) {
	m := New()
	code, _ := m.IssueSetupCode(9)
	var wg sync.WaitGroup
	wins := make(chan uint, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, ok := m.TakeSetupCode(code); ok {
				wins <- id
			}
		}()
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d fetches spent one code", n)
	}
}

// Deleting an entry leaves nothing of it in memory: not its list digests,
// not its codes, not what was seen of its setup; another entry keeps its own.
func TestForgetLeavesNothingUnderTheEntry(t *testing.T) {
	m, _ := atClock(t)
	st := &fakeReportStore{}
	for _, id := range []uint{1, 2} {
		if _, err := m.Ingest(st, id, proto.SlurmGPUNodeReport{Digest: "d", Nodes: []proto.SlurmGPUNodeEntry{{Name: "gpu0", State: "idle", GPUs: 1}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.IssueSetupCode(id); err != nil {
			t.Fatal(err)
		}
		m.SetupRan(id)
		m.NodeAsked(id)
		m.Reached(id, provider.SlurmReach{Hosts: []provider.SlurmReachHost{{Role: "node", Name: "n", Status: "failed"}}})
	}
	discarded, _ := m.IssueSetupCode(1)
	m.DiscardSetupCode(discarded)
	if !m.holds(1) || !m.holds(2) {
		t.Fatal("nothing was kept to forget")
	}
	m.Forget(1)
	m.Forget(9) // an entry never seen
	if m.holds(1) {
		t.Fatal("a deleted entry is still in memory")
	}
	if !m.holds(2) {
		t.Fatal("forgetting one entry dropped another")
	}
	m.ListChanged(2)
	m.mu.Lock()
	_, digests := m.reports[2]
	_, seen := m.seen[2]
	m.mu.Unlock()
	if digests || !seen {
		t.Fatalf("ListChanged dropped digests %v, kept what was seen %v", !digests, seen)
	}
}

func intp(n int) *int { return &n }

// The waiting reason: a cloud with no compute nodes first, then an
// unreachable server, then what was seen of the setup; nothing is claimed
// of an entry from before the server watched.
func TestWaitingReasons(t *testing.T) {
	down := provider.SlurmReach{Hosts: []provider.SlurmReachHost{{Role: "node", Name: "gpu000", Status: "failed", Reason: "timeout"}}}
	for _, tc := range []struct {
		name    string
		arrange func(m *Memory, now *time.Time)
		before  bool // created before the server watched
		nodes   *int
		want    string
		joining bool
	}{
		{"nothing seen", nil, false, nil, WaitingNotApplied, false},
		{"setup seen a minute ago", func(m *Memory, now *time.Time) { m.SetupRan(1); *now = now.Add(time.Minute) }, false, nil, WaitingJoining, true},
		{"setup seen long ago", func(m *Memory, now *time.Time) { m.SetupRan(1); *now = now.Add(JoinWindow) }, false, nil, WaitingNoNodeCalled, false},
		{"a node called", func(m *Memory, _ *time.Time) { m.SetupRan(1); m.NodeAsked(1) }, false, nil, "", true},
		{"no compute nodes outranks the rest", func(m *Memory, _ *time.Time) { m.SetupRan(1); m.Reached(1, down) }, false, intp(0), WaitingNoComputeNodes, true},
		{"unreachable outranks no node called", func(m *Memory, now *time.Time) { m.SetupRan(1); m.Reached(1, down); *now = now.Add(JoinWindow) }, false, intp(2), WaitingServerUnreachable, false},
		{"unreachable is a finding, not a guess", func(m *Memory, _ *time.Time) { m.Reached(1, down) }, true, nil, WaitingServerUnreachable, false},
		{"before the server watched nothing is claimed", nil, true, nil, "", false},
		{"before the server watched, a setup is not guessed either", func(m *Memory, _ *time.Time) { m.SetupRan(1) }, true, intp(1), "", false},
	} {
		m, now := atClock(t)
		c := store.SlurmCluster{ID: 1, CreatedAt: *now}
		if tc.before {
			c.CreatedAt = now.Add(-time.Hour)
		}
		if tc.arrange != nil {
			tc.arrange(m, now)
		}
		got := m.Waiting(c, tc.nodes)
		if got.Reason != tc.want || got.Joining != tc.joining {
			t.Errorf("%s: reason %q joining %v, want %q %v", tc.name, got.Reason, got.Joining, tc.want, tc.joining)
		}
		if tc.want == WaitingServerUnreachable && len(got.Hosts) != 1 {
			t.Errorf("%s: hosts = %v", tc.name, got.Hosts)
		}
	}
	// A later run that reached the server clears the finding.
	m, now := atClock(t)
	c := store.SlurmCluster{ID: 1, CreatedAt: *now}
	m.Reached(1, down)
	m.Reached(1, provider.SlurmReach{Hosts: []provider.SlurmReachHost{{Role: "node", Name: "gpu000", Status: "ok"}}})
	if got := m.Waiting(c, nil); got.Reason != WaitingNotApplied {
		t.Fatalf("after a run that reached the server = %+v", got)
	}
}

// A stalled entry is one no node enrolled in for too long, unless it is
// joining; enrolling or revoking ends it.
func TestEntryStates(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	old := now.Add(-2 * StalledAfter)
	for _, tc := range []struct {
		name    string
		c       store.SlurmCluster
		joining bool
		state   string
	}{
		{"new", store.SlurmCluster{CreatedAt: now}, false, StatePending},
		{"waiting too long", store.SlurmCluster{CreatedAt: old}, false, StateStalled},
		{"waiting long, but joining", store.SlurmCluster{CreatedAt: old}, true, StatePending},
		{"enrolled", store.SlurmCluster{CreatedAt: old, SlurmClusterName: "demo"}, false, StateActive},
		{"revoked", store.SlurmCluster{CreatedAt: old, RevokedAt: &now}, false, StateRevoked},
	} {
		if got := State(tc.c, Stalled(tc.c, now, tc.joining)); got != tc.state {
			t.Errorf("%s: state = %s, want %s", tc.name, got, tc.state)
		}
	}
}
