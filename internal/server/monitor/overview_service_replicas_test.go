package monitor

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// replicaFixture is a deployment of replicas spread two per node, each owning
// gpus consecutive GPUs, with every signal a replica row reads written at a
// value derived from the replica / GPU position so a row that read another
// replica's or another GPU's series fails loudly.
type replicaFixture struct {
	replicas int
	gpus     int
	// unlabeled lists replica positions whose inference series carry only
	// "instance" (an agent that predates the "deployment" label).
	unlabeled map[int]bool
	// noData lists replica positions that export nothing at all.
	noData map[int]bool
	// noWorkerSeries leaves the headless worker rank's engine series out.
	noWorkerSeries bool
}

func (f replicaFixture) uuid(k int) string { return fmt.Sprintf("i%d", k) }
func (f replicaFixture) node(k int) string { return fmt.Sprintf("n%d", k/2) }
func (f replicaFixture) gpuIndexes(k int) []int {
	out := make([]int, f.gpus)
	for g := range out {
		out[g] = (k%2)*f.gpus + g
	}
	return out
}

func (f replicaFixture) inventory() Inventory {
	inv := Inventory{Deployments: []DeploymentRef{{ID: 1, Name: "svc"}, {ID: 2, Name: "other"}}}
	for k := 0; k < f.replicas; k++ {
		inv.Instances = append(inv.Instances, InstanceRef{
			UUID: f.uuid(k), DeploymentID: 1, NodeUUID: f.node(k), State: "running", GPUIndexes: f.gpuIndexes(k),
		})
		if k%2 == 0 {
			inv.Nodes = append(inv.Nodes, NodeRef{UUID: f.node(k), Name: "node-" + f.node(k)})
		}
	}
	// A headless worker rank gets no row and its series must not leak in.
	inv.Instances = append(inv.Instances, InstanceRef{UUID: "worker", DeploymentID: 1, NodeUUID: "n0", Headless: true, GPUIndexes: []int{1000}})
	// A sibling deployment's replica on a node of this one, on other GPUs.
	inv.Instances = append(inv.Instances, InstanceRef{UUID: "stranger", DeploymentID: 2, NodeUUID: "n0", GPUIndexes: []int{1001}})
	return inv
}

// write seeds six 10 s samples ending at baseTime. alt alternates between
// lo and hi (three of each, so the window mean is their midpoint); counters
// advance by perStep each sample; ramps rise by perStep (so the max is the
// last value).
func (f replicaFixture) write(t testing.TB, store tsdb.Store) {
	t.Helper()
	start := baseTime.Add(-50 * time.Second)
	var batch []tsdb.Sample
	series := func(metric string, labels map[string]string, value func(i int) float64) {
		for i := 0; i < 6; i++ {
			at := start.Add(time.Duration(i) * 10 * time.Second)
			batch = append(batch, tsdb.Sample{Metric: metric, Labels: labels, Value: value(i), TimeMS: at.UnixMilli()})
		}
	}
	alt := func(metric string, labels map[string]string, lo, hi float64) {
		series(metric, labels, func(i int) float64 {
			if i%2 == 1 {
				return hi
			}
			return lo
		})
	}
	counter := func(metric string, labels map[string]string, perStep float64) {
		series(metric, labels, func(i int) float64 { return 1000 + perStep*float64(i) })
	}
	ramp := func(metric string, labels map[string]string, from, perStep float64) {
		series(metric, labels, func(i int) float64 { return from + perStep*float64(i) })
	}

	for k := 0; k < f.replicas; k++ {
		if !f.noData[k] {
			inst := map[string]string{"instance": f.uuid(k)}
			if !f.unlabeled[k] {
				inst["deployment"] = "1"
			}
			alt("gpuworks_infer_requests_running", inst, float64(k+1), float64(k+3))
			alt("gpuworks_infer_requests_waiting", inst, 0, float64(k%3)*2)
			ramp("gpuworks_infer_gpu_cache_usage_percent", inst, 0.1+0.01*float64(k), 0.001)
			counter("gpuworks_infer_kv_queries_total", inst, 100)
			counter("gpuworks_infer_kv_hits_total", inst, 40*float64(k%2+1))
			counter("gpuworks_infer_prompt_tokens_total", inst, 50*float64(k+1))
			counter("gpuworks_infer_generation_tokens_total", inst, 20*float64(k+1))
		}
		for _, g := range f.gpuIndexes(k) {
			gpu := map[string]string{"node": f.node(k), "gpu": fmt.Sprint(g)}
			base := f.gpuBase(k, g)
			alt("gpuworks_gpu_utilization", gpu, base-2, base+2)
			alt(metricGPUMemBW, gpu, base/2-1, base/2+1)
			ramp("gpuworks_gpu_temperature_celsius", gpu, 40+base, 1)
			alt("gpuworks_gpu_power_watts", gpu, 100+base, 120+base)
		}
	}
	// Series the rows must not pick up.
	other := map[string]string{"instance": "stranger", "deployment": "2"}
	alt("gpuworks_infer_requests_running", other, 900, 900)
	counter("gpuworks_infer_prompt_tokens_total", other, 9000)
	if !f.noWorkerSeries {
		alt("gpuworks_infer_requests_running", map[string]string{"instance": "worker", "deployment": "1"}, 800, 800)
	}
	for _, g := range []int{1000, 1001} {
		alt("gpuworks_gpu_utilization", map[string]string{"node": "n0", "gpu": fmt.Sprint(g)}, 99, 99)
		alt("gpuworks_gpu_power_watts", map[string]string{"node": "n0", "gpu": fmt.Sprint(g)}, 999, 999)
	}
	if err := store.WriteBatch(batch); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// gpuBase is the value GPU g of replica k's hardware signals are derived from.
func (f replicaFixture) gpuBase(k, g int) float64 { return float64(10 + 5*(k/2) + g) }

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// expectReplicaRow asserts the row for replica k holds exactly what the
// fixture wrote for it.
func (f replicaFixture) expectReplicaRow(t testing.TB, k int, row ReplicaRow) {
	t.Helper()
	name := f.uuid(k)
	if row.InstanceUUID != name || row.NodeUUID != f.node(k) || row.NodeName != "node-"+f.node(k) || row.State != "running" {
		t.Fatalf("%s identity = %+v", name, row)
	}
	if len(row.GPUs) != f.gpus {
		t.Fatalf("%s has %d GPU rows, want %d", name, len(row.GPUs), f.gpus)
	}
	if f.noData[k] {
		if row.HasRunning || row.HasWaiting || row.HasKVUsage || row.HasKVHitRate || row.HasTokens {
			t.Errorf("%s exports nothing but row = %+v", name, row)
		}
	} else {
		for _, c := range []struct {
			label     string
			has       bool
			got, want float64
		}{
			{"running", row.HasRunning, row.Running, float64(k + 2)},
			{"waiting", row.HasWaiting, row.Waiting, float64(k % 3)},
			{"kv usage", row.HasKVUsage, row.KVUsage, 0.1 + 0.01*float64(k) + 0.005},
			{"kv hit rate", row.HasKVHitRate, row.KVHitRate, 0.4 * float64(k%2+1)},
			{"prompt tok/s", row.HasTokens, row.PromptTokensPerSec, 5 * float64(k+1)},
			{"gen tok/s", row.HasTokens, row.GenTokensPerSec, 2 * float64(k+1)},
		} {
			if !c.has || !approxEqual(c.got, c.want) {
				t.Errorf("%s %s = %v (has=%v), want %v", name, c.label, c.got, c.has, c.want)
			}
		}
	}
	for i, g := range f.gpuIndexes(k) {
		base := f.gpuBase(k, g)
		gpu := row.GPUs[i]
		if gpu.Index != g || !gpu.HasUtil || !approxEqual(gpu.UtilPct, base) ||
			!gpu.HasMemBW || !approxEqual(gpu.MemBWPct, base/2) ||
			!gpu.HasTemp || !approxEqual(gpu.TempC, 45+base) ||
			!gpu.HasPower || !approxEqual(gpu.PowerW, 110+base) {
			t.Errorf("%s gpu %d = %+v, want util %v memBW %v temp %v power %v", name, g, gpu, base, base/2, 45+base, 110+base)
		}
	}
}

// TestBuildServiceReplicaRowsHoldEachReplicasOwnSignals pins the replica
// table's values for a multi-node, multi-GPU deployment: every row carries
// its own replica's inference signals and its own GPUs' hardware signals,
// whether the replica's series carry the deployment label or only the
// instance (older agent), and a replica exporting nothing stays empty.
func TestBuildServiceReplicaRowsHoldEachReplicasOwnSignals(t *testing.T) {
	f := replicaFixture{replicas: 8, gpus: 2, unlabeled: map[int]bool{3: true}, noData: map[int]bool{5: true}}
	store := newTestStore(t)
	f.write(t, store)
	deps := Deps{TSDB: store, Inventory: inventoryFunc(f.inventory()), Now: func() time.Time { return baseTime }}

	snap, err := BuildService(context.Background(), deps, 1, 15*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Replicas) != f.replicas {
		t.Fatalf("Replicas = %d, want %d (headless worker excluded)", len(snap.Replicas), f.replicas)
	}
	for k, row := range snap.Replicas {
		f.expectReplicaRow(t, k, row)
	}
}

// TestBuildServiceReplicaRowsIgnoreGatewayStandIns proves a deployment whose
// engine exports nothing (the window and charts fall back to the gateway's
// deployment-wide figures) still shows its replicas empty: the table reports
// what each replica's engine exports, never a deployment-wide stand-in.
func TestBuildServiceReplicaRowsIgnoreGatewayStandIns(t *testing.T) {
	f := replicaFixture{replicas: 4, gpus: 1, noData: map[int]bool{0: true, 1: true, 2: true, 3: true}, noWorkerSeries: true}
	store := newTestStore(t)
	f.write(t, store)
	for i := 0; i < 6; i++ {
		writeSample(t, store, metricProxyInflight, map[string]string{"deployment": "1"}, 7, baseTime.Add(time.Duration(i-5)*10*time.Second))
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(f.inventory()), Now: func() time.Time { return baseTime }}

	snap, err := BuildService(context.Background(), deps, 1, 15*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if snap.Sources[SignalRunning] != SourceGateway {
		t.Fatalf("Sources = %v, want running from the gateway (fixture precondition)", snap.Sources)
	}
	for k, row := range snap.Replicas {
		f.expectReplicaRow(t, k, row)
	}
}

// countingStore counts QueryRange calls, to prove a path's query count does
// not follow the replica count.
type countingStore struct {
	tsdb.Store
	queries int
}

func (c *countingStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	c.queries++
	return c.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

func serviceQueryCount(t testing.TB, f replicaFixture) int {
	t.Helper()
	store := newTestStore(t)
	f.write(t, store)
	counting := &countingStore{Store: store}
	deps := Deps{TSDB: counting, Inventory: inventoryFunc(f.inventory()), Now: func() time.Time { return baseTime }}
	snap, err := BuildService(context.Background(), deps, 1, 15*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Replicas) != f.replicas {
		t.Fatalf("Replicas = %d, want %d", len(snap.Replicas), f.replicas)
	}
	return counting.queries
}

// TestBuildServiceQueryCountDoesNotGrowWithReplicas is the scale guard for
// the deployment page: the number of tsdb queries is a constant of the
// metric catalog, independent of how many replicas or GPUs the deployment
// has (it used to issue ~8 queries per replica plus 4 per GPU, each GPU
// query scanning every GPU series in the fleet).
func TestBuildServiceQueryCountDoesNotGrowWithReplicas(t *testing.T) {
	small := serviceQueryCount(t, replicaFixture{replicas: 4, gpus: 1})
	large := serviceQueryCount(t, replicaFixture{replicas: 64, gpus: 1})
	manyGPUs := serviceQueryCount(t, replicaFixture{replicas: 64, gpus: 8})
	t.Logf("BuildService tsdb queries: 4x1=%d 64x1=%d 64x8=%d", small, large, manyGPUs)
	if small != large || large != manyGPUs {
		t.Fatalf("BuildService tsdb queries = %d (4x1), %d (64x1), %d (64x8); want identical", small, large, manyGPUs)
	}
	const budget = 60
	if small > budget {
		t.Fatalf("BuildService issued %d tsdb queries, want at most %d", small, budget)
	}
}

func BenchmarkBuildServiceReplicas(b *testing.B) {
	f := replicaFixture{replicas: 24, gpus: 1}
	store := newTestStore(b)
	f.write(b, store)
	deps := Deps{TSDB: store, Inventory: inventoryFunc(f.inventory()), Now: func() time.Time { return baseTime }}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildService(context.Background(), deps, 1, 15*time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}
