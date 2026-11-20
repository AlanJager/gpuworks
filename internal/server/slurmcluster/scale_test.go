package slurmcluster

import (
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// countingStore is the real store, counting the writes of entries' lists.
type countingStore struct {
	*store.Store
	writes, counts atomic.Int64
}

func (c *countingStore) ReplaceSlurmGPUNodes(cluster uint, nodes []store.SlurmGPUNode) (store.SlurmListChange, error) {
	c.writes.Add(1)
	return c.Store.ReplaceSlurmGPUNodes(cluster, nodes)
}

func (c *countingStore) SlurmListRowCount(cluster uint) (int, error) {
	c.counts.Add(1)
	return c.Store.SlurmListRowCount(cluster)
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// One entry of a 1000-node cluster, every node's agent reporting once per
// ReportInterval, on a busy cluster where states change all the time, so no
// two reports carry the same state digest: over half an hour
// the server writes the list at most once a minute, answers every other
// report with no write, and the whole run stays within a CPU budget. The
// write cap is what keeps a busy cluster from rewriting a thousand rows with
// every report (go test ./internal/server/slurmcluster/ -run Scale -v).
func TestIngestScaleThousandAgentsOfABusyCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("scale case")
	}
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	entry, err := st.CreateSlurmCluster(store.SlurmCluster{Name: "busy", ServerURL: "https://gw", RunAs: "root",
		InstallDir: store.SlurmDefaultInstallDir, ScriptDir: store.SlurmDefaultScriptDir,
		IdleReturnSeconds: store.SlurmDefaultIdleReturnSeconds, PendingTimeoutSeconds: store.SlurmDefaultPendingTimeoutSeconds})
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: st}
	m, now := atClock(t)

	const agents, minutes = 1000, 30
	states := []string{"idle", "mixed", "allocated"}
	list := func(minute int) []proto.SlurmGPUNodeEntry {
		out := make([]proto.SlurmGPUNodeEntry, agents)
		for i := range out {
			out[i] = proto.SlurmGPUNodeEntry{Name: fmt.Sprintf("gpu%04d", i), State: states[(i+minute)%len(states)], GPUs: 8}
		}
		return out
	}
	perMinute := agents / int(ReportInterval/time.Minute)
	start := cpuTime()
	reports := 0
	for minute := 0; minute < minutes; minute++ {
		for a := 0; a < perMinute; a++ {
			*now = now.Add(time.Minute / time.Duration(perMinute))
			reports++
			// Each agent lists the cluster at its own moment: on a busy
			// cluster no two reports see the same states.
			rep := proto.SlurmGPUNodeReport{Digest: "members", StateDigest: fmt.Sprint("states-", reports)}
			need, err := m.Ingest(cs, entry.ID, rep)
			if err != nil {
				t.Fatal(err)
			}
			if need {
				full := rep
				full.Nodes = list(reports)
				if _, err := m.Ingest(cs, entry.ID, full); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	spent := cpuTime() - start
	writes := cs.writes.Load()
	t.Logf("%d reports over %d minutes: %d list writes, %d row counts, %v CPU", reports, minutes, writes, cs.counts.Load(), spent)
	if writes > minutes+1 {
		t.Fatalf("%d list writes in %d minutes; want at most one a minute", writes, minutes)
	}
	if writes < minutes/2 {
		t.Fatalf("only %d list writes in %d minutes of changing states: the list is not kept up to date", writes, minutes)
	}
	if !raceDetector && spent > 15*time.Second {
		t.Fatalf("%v CPU for %d reports of a %d-node cluster", spent, reports, agents)
	}
	names, err := st.SlurmGPUNodeNames(entry.ID)
	if err != nil || len(names) != agents {
		t.Fatalf("stored %d GPU nodes, %v", len(names), err)
	}
}
