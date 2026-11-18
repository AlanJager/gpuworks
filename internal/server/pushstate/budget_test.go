package pushstate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The write budget of a large push (plan acceptance A9), at 1000 nodes on a
// database file:
//
//   - a full replace that changes 1% of a table writes about twice the
//     changed entries (a delete and an insert each) and a few rows more;
//   - the writer holds the database for under budgetWriterHoldP99 per tick
//     at the 99th percentile;
//   - status reports written at the same time wait under
//     budgetStatusWaitP99 longer at the 99th percentile than without the
//     pushes.
const (
	budgetNodes         = 1000
	budgetPushes        = 60
	budgetRowsConstant  = 4
	budgetWriterHoldP99 = 20 * time.Millisecond
	budgetStatusWaitP99 = 5 * time.Millisecond
	budgetTickInterval  = 50 * time.Millisecond
)

// timedStore records how long each commit of pushed state held the writer
// and what it wrote.
type timedStore struct {
	*faultyStore
	mu    sync.Mutex
	holds []time.Duration
	rows  []int
}

func (s *timedStore) WritePushedState(ctx context.Context, b *pstate.Batch) (pstate.Result, error) {
	start := time.Now()
	res, err := s.faultyStore.WritePushedState(ctx, b)
	held := time.Since(start)
	if len(b.Tables) > 0 {
		s.mu.Lock()
		s.holds = append(s.holds, held)
		s.rows = append(s.rows, rowsWritten(b))
		s.mu.Unlock()
	}
	return res, err
}

func (s *timedStore) take() ([]time.Duration, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, r := s.holds, s.rows
	s.holds, s.rows = nil, nil
	return h, r
}

// rowsWritten counts the rows a batch's table writes touch: each owner
// record, each entry deleted, and every row inserted (every slice of
// pstate.Rows, so a row type added later is counted too).
func rowsWritten(b *pstate.Batch) int {
	n := 0
	for _, w := range b.Tables {
		n++ // the owner record
		if w.DeleteAll {
			n++
		}
		n += len(w.DeleteEntries)
		v := reflect.ValueOf(w.Insert)
		for i := range v.NumField() {
			n += v.Field(i).Len()
		}
	}
	return n
}

// budgetHolder is the scheduling holder the app registers (package app's
// PushedStateHooks.Holder), so every status write asks pushed state for its
// hold inside the writer's transaction, as it does in the server.
type budgetHolder struct{ s Evaluator }

func (budgetHolder) Source() store.SchedulingSource { return HoldSource }
func (h budgetHolder) Hold(f store.NodeHoldFacts) store.NodeHold {
	return store.NodeHold(h.s.Hold(f.ID, f.OwnedCards))
}

// holdsBody is round's node_holds table over nodes 1..size: the entries
// fall into slices of 1%, and round r flips slice r between two codes, so
// consecutive rounds differ in one slice (the fake plugin's --push-every
// pushes the same tables).
func holdsBody(size, round int) string {
	per := max(1, size/100)
	slices := (size + per - 1) / per
	var b strings.Builder
	b.WriteString(`{"nodes":{`)
	for i := 0; i < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		flips := 0
		if s := i / per; round >= s {
			flips = (round-s)/slices + 1
		}
		code := "ACME_MAINTENANCE"
		if flips%2 == 1 {
			code = "ACME_QUARANTINE"
		}
		b.WriteString(`"` + strconv.Itoa(i+1) + `":{"code":"` + code + `"}`)
	}
	b.WriteString(`}}`)
	return b.String()
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(float64(len(s)-1) * p)
	return s[i]
}

// statusReports writes one status report after another, round robin over
// uuids, until stop is closed, and returns how long each took.
func statusReports(t *testing.T, st *store.Store, uuids []string, stop <-chan struct{}) []time.Duration {
	var out []time.Duration
	ok := true
	for i := 0; ; i++ {
		select {
		case <-stop:
			return out
		default:
		}
		start := time.Now()
		if err := st.UpdateNodeStatus(uuids[i%len(uuids)], budgetStatus(uuids[i%len(uuids)], &ok, i)); err != nil {
			t.Errorf("status report: %v", err)
			return out
		}
		out = append(out, time.Since(start))
	}
}

// budgetStatus is a status report of the size a node with eight cards
// sends; seq changes its free memory so every report is a real write.
func budgetStatus(uuid string, ok *bool, seq int) proto.NodeStatus {
	gpus := make([]proto.GPUInfo, 8)
	for i := range gpus {
		gpus[i] = proto.GPUInfo{Index: i, UUID: fmt.Sprintf("GPU-%s-%d", uuid, i), Vendor: "nvidia", Name: "A100",
			PCIeBDF: fmt.Sprintf("0000:%02x:00.0", 0x30+i), MemoryTotalBytes: 80 << 30, Healthy: true}
	}
	return proto.NodeStatus{GPUProbeOK: ok, GPUs: gpus, Sys: proto.SysInfo{RuntimeDriver: "docker",
		MemTotalKB: 512 << 20, MemAvailKB: int64(256+seq%128) << 20}}
}

// TestPushedStateWriteBudget measures what an authority plugin's large
// pushes cost the writer. It is a load measurement on a database file, so
// it runs on request on an idle host:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -run '^TestPushedStateWriteBudget$' ./internal/server/pushstate/
func TestPushedStateWriteBudget(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("write budget on a database file; set GPUWORKS_TEST_PERF=1 on an idle host to run (full command in the test's doc comment)")
	}
	fs := openStore(t, filepath.Join(t.TempDir(), "gpuworks.db"))
	uuids := make([]string, budgetNodes)
	ok := true
	for i := range uuids {
		n, _, err := fs.CreateNodePending(fmt.Sprintf("budget-%04d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		if n.ID != uint(i+1) {
			t.Fatalf("node %d has id %d; the holds name ids 1..%d", i+1, n.ID, budgetNodes)
		}
		uuids[i] = n.UUID
		if err := fs.UpdateNodeStatus(n.UUID, budgetStatus(n.UUID, &ok, 0)); err != nil {
			t.Fatal(err)
		}
	}
	ts := &timedStore{faultyStore: fs}
	var reapplied atomic.Int32
	h := newHarness(t, fs, newFakeClock("2026-12-01T00:00:00Z"), func(c *Config) {
		c.Store, c.TickInterval = ts, budgetTickInterval
		c.Effects.ReapplyHolds = func() { reapplied.Add(1) }
	}).load()
	if err := fs.AddSchedulingHolder(budgetHolder{s: h.s}); err != nil {
		t.Fatal(err)
	}
	h.run()
	ops := pusherOf(acmeOps, "push:node_holds")
	gen := uint64(1)
	h.push(ops, extapi.KindNodeHolds, gen, holdsBody(budgetNodes, 0))
	if first, rows := ts.take(); len(first) != 1 || rows[0] < budgetNodes {
		t.Fatalf("the first push wrote %v rows in %d commits; want the whole table in one", rows, len(first))
	}
	if !h.s.Hold(1, 8).held() || !h.s.Hold(budgetNodes, 8).held() {
		t.Fatal("the first push holds no node")
	}

	// Each push continues the sequence, so it differs from the stored table
	// in one slice.
	round := 0
	pushing := func(stop chan struct{}) time.Duration {
		start := time.Now()
		for range budgetPushes {
			gen++
			round++
			h.push(ops, extapi.KindNodeHolds, gen, holdsBody(budgetNodes, round))
		}
		close(stop)
		return time.Since(start)
	}

	// The same push sequence once without status reports gives the window;
	// status reports alone over that window are the baseline.
	window := pushing(make(chan struct{}))
	ts.take()
	stop := make(chan struct{})
	go func() { time.Sleep(window); close(stop) }()
	baseline := statusReports(t, fs.Store, uuids, stop)

	stop = make(chan struct{})
	reports := make(chan []time.Duration, 1)
	go func() { reports <- statusReports(t, fs.Store, uuids, stop) }()
	pushing(stop)
	loaded := <-reports
	holds, rows := ts.take()

	changed := max(1, budgetNodes/100)
	t.Logf("nodes=%d pushes=%d changed per push=%d window=%s", budgetNodes, budgetPushes, changed, window.Round(time.Millisecond))
	t.Logf("rows written per push: max %d (budget %d)", slices.Max(rows), 2*changed+budgetRowsConstant)
	t.Logf("writer hold per tick: p50 %s p99 %s max %s (budget p99 %s)", percentile(holds, 0.5), percentile(holds, 0.99), slices.Max(holds), budgetWriterHoldP99)
	t.Logf("status write: baseline n=%d p50 %s p99 %s; with pushes n=%d p50 %s p99 %s (budget +%s)",
		len(baseline), percentile(baseline, 0.5), percentile(baseline, 0.99), len(loaded), percentile(loaded, 0.5), percentile(loaded, 0.99), budgetStatusWaitP99)

	if len(holds) < budgetPushes {
		t.Fatalf("%d commits for %d pushes", len(holds), budgetPushes)
	}
	for i, n := range rows {
		if n > 2*changed+budgetRowsConstant {
			t.Errorf("push %d wrote %d rows for %d changed entries; budget %d", i+1, n, changed, 2*changed+budgetRowsConstant)
		}
	}
	if p := percentile(holds, 0.99); p > budgetWriterHoldP99 {
		t.Errorf("writer hold p99 %s, budget %s", p, budgetWriterHoldP99)
	}
	if len(baseline) < 100 || len(loaded) < 100 {
		t.Fatalf("too few status writes to compare: %d without pushes, %d with", len(baseline), len(loaded))
	}
	if added := percentile(loaded, 0.99) - percentile(baseline, 0.99); added > budgetStatusWaitP99 {
		t.Errorf("status writes wait %s longer at p99 with the pushes, budget %s", added, budgetStatusWaitP99)
	}
	if reapplied.Load() == 0 {
		t.Error("changed holds never asked for a reapply")
	}
}
