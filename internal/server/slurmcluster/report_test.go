package slurmcluster

import (
	"errors"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeReportStore is the store as ingestion uses it: rows it stored per
// entry, how often it was asked to replace them, and failures to inject.
type fakeReportStore struct {
	mu       sync.Mutex
	rows     map[uint]int
	replaces int
	counts   int
	delay    time.Duration
	err      error
}

func (f *fakeReportStore) SlurmListRowCount(cluster uint) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts++
	return f.rows[cluster], f.err
}

func (f *fakeReportStore) ReplaceSlurmGPUNodes(cluster uint, nodes []store.SlurmGPUNode) (store.SlurmListChange, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.SlurmListChange{}, f.err
	}
	f.replaces++
	if f.rows == nil {
		f.rows = map[uint]int{}
	}
	f.rows[cluster] = len(nodes)
	return store.SlurmListChange{Replaced: true}, nil
}

func nodes(names ...string) []proto.SlurmGPUNodeEntry {
	out := make([]proto.SlurmGPUNodeEntry, 0, len(names))
	for _, n := range names {
		out = append(out, proto.SlurmGPUNodeEntry{Name: n, State: "idle", GPUs: 1})
	}
	return out
}

// The digests alone ask for the list; the list is stored; the same digests
// again cost one count and no write; a change waits a minute after a write;
// a row removed outside a report has the list stored whole again.
func TestIngestStoresAListOnceAndAtMostOncePerInterval(t *testing.T) {
	m, now := atClock(t)
	st := &fakeReportStore{}
	need, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "a"})
	if err != nil || !need {
		t.Fatalf("new digests = %v, %v; want the list asked for", need, err)
	}
	if need, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "a", Nodes: nodes("gpu0", "gpu1")}); err != nil || need || st.replaces != 1 {
		t.Fatalf("the list = %v, %v, %d writes", need, err, st.replaces)
	}
	if need, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "a"}); err != nil || need || st.replaces != 1 || st.counts != 1 {
		t.Fatalf("the same digests = %v, %v, %d writes, %d counts", need, err, st.replaces, st.counts)
	}
	// A change right after the write waits.
	*now = now.Add(StateWriteInterval - time.Second)
	if need, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "b"}); err != nil || need {
		t.Fatalf("a change inside the interval = %v, %v; want it to wait", need, err)
	}
	*now = now.Add(time.Second)
	if need, _ := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "b"}); !need {
		t.Fatal("a change after the interval was not asked for")
	}
	if _, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "b", Nodes: nodes("gpu0")}); err != nil || st.replaces != 2 {
		t.Fatalf("second write: %v, %d writes", err, st.replaces)
	}
	// A node deleted by hand took its row: the same digests store the list again.
	st.rows[1] = 0
	*now = now.Add(time.Second)
	if need, _ := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "b"}); !need {
		t.Fatal("a list missing a row was not asked for again")
	}
}

func TestIngestRefusesABadListAndPassesStoreErrors(t *testing.T) {
	m, _ := atClock(t)
	st := &fakeReportStore{}
	for _, list := range [][]proto.SlurmGPUNodeEntry{nodes("gpu0", "gpu0"), nodes(""), nodes("this-node-name-is-far-too-long-to-be-a-slurm-node-name-of-at-most-64")} {
		_, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "x", Nodes: list})
		var bad *BadNodesError
		if !errors.As(err, &bad) || bad.Reason == "" {
			t.Fatalf("list %v = %v; want a BadNodesError", list, err)
		}
	}
	if st.replaces != 0 {
		t.Fatal("a bad list was stored")
	}
	st.err = errors.New("disk full")
	if _, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "x", Nodes: nodes("gpu0")}); !errors.Is(err, st.err) {
		t.Fatalf("store failure = %v", err)
	}
	// A failed write remembers nothing: the next report asks for the list again.
	st.err = nil
	if need, _ := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "x"}); !need {
		t.Fatal("after a failed write the list was not asked for again")
	}
}

// Every agent of an entry reports; when many find the interval over at the
// same moment, one of them writes and the others see that it did.
func TestIngestConcurrentReportsOfOneEntryWriteOnce(t *testing.T) {
	m, now := atClock(t)
	st := &fakeReportStore{delay: 5 * time.Millisecond}
	if _, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "a", Nodes: nodes("gpu0")}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * StateWriteInterval)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Ingest(st, 1, proto.SlurmGPUNodeReport{Digest: "b", StateDigest: "s", Nodes: nodes("gpu0", "gpu1")}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if st.replaces != 2 {
		t.Fatalf("%d writes for one change reported by 32 agents at once; want 1 more than the first", st.replaces)
	}
}
