package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// The fleet the scheduling center is sized for (CLAUDE.md): 1000 nodes of 8
// cards and 3000 instances. These are store-level rows, written directly, which
// is what the overview reads; the full-process fleet cases (test/l2) do not
// exercise this endpoint.
const (
	scaleNodes     = 1000
	scaleCards     = 8
	scaleInstances = 3000
	scaleDeploys   = 700
)

type scaleFleet struct {
	rig     *tokenRig
	a       *apiHandlers
	deps    Deps
	session string
}

// sharingStatementBudget is how many statements a cold read of the fleet's
// sharing stats may run: a fixed set, never one per deployment or instance.
const sharingStatementBudget = 100

// overviewStatementBudget is how many statements a rebuild of the overview
// may run: a fixed set, never one per node, card or instance.
const overviewStatementBudget = 100

// sessionStatements is how many statements a request's session check runs.
const sessionStatements = 5

func seedScaleFleet(t *testing.T) *scaleFleet {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	db := rig.st.DB()
	now := time.Now().UTC()
	models := make([]store.Model, 20)
	for i := range models {
		models[i] = store.Model{Name: fmt.Sprintf("m%d", i), Source: "huggingface", SourceID: fmt.Sprintf("org/m%d", i), WeightBytes: int64(8+i) << 30, Dtype: "bfloat16"}
	}
	if err := db.CreateInBatches(&models, 50).Error; err != nil {
		t.Fatal(err)
	}
	levels := []string{yieldpolicy.Never, yieldpolicy.Idle, yieldpolicy.TimeSlice, yieldpolicy.Anytime}
	deployments := make([]store.Deployment, scaleDeploys)
	for i := range deployments {
		deployments[i] = store.Deployment{
			Name: fmt.Sprintf("dep-%03d", i), ServedName: fmt.Sprintf("dep-%03d", i), ModelID: models[i%len(models)].ID, Engine: "vllm", Sleepable: true,
			State: store.DeployActive, Replicas: 1,
		}
		presetDeployment(&deployments[i], levels[i%len(levels)])
	}
	if err := db.CreateInBatches(&deployments, 200).Error; err != nil {
		t.Fatal(err)
	}
	nodes := make([]store.Node, scaleNodes)
	for i := range nodes {
		nodes[i] = store.Node{
			UUID: fmt.Sprintf("node-%04d", i), Name: fmt.Sprintf("gpu-%04d", i), Token: fmt.Sprintf("tok-%04d", i), TokenHash: fmt.Sprintf("hash-%04d", i), State: store.NodeStateOnline,
			Hostname: fmt.Sprintf("host-%04d", i), Labels: fmt.Sprintf(`{"zone":"z%d","rack":"r%d"}`, i%4, i%50),
		}
		if i%25 == 0 {
			nodes[i].State = store.NodeStateOffline
		}
	}
	if err := db.CreateInBatches(&nodes, 200).Error; err != nil {
		t.Fatal(err)
	}
	cards := make([]store.GPU, 0, scaleNodes*scaleCards)
	for i := range nodes {
		name, mem := "L20", int64(48)<<30
		if i%3 == 0 {
			name, mem = "H100", int64(80)<<30
		}
		for c := 0; c < scaleCards; c++ {
			card := store.GPU{NodeID: nodes[i].ID, Ident: fmt.Sprintf("n%d-%d", i, c), Index: c, UUID: fmt.Sprintf("GPU-%d-%d", i, c), Name: name, Vendor: "nvidia", MemoryTotal: mem}
			if (i*scaleCards+c)%97 == 0 {
				card.ForeignProcesses = `[{"pid":1,"name":"python"}]`
			}
			cards = append(cards, card)
		}
	}
	if err := db.CreateInBatches(&cards, 500).Error; err != nil {
		t.Fatal(err)
	}
	instances := make([]store.Instance, scaleInstances)
	for i := range instances {
		node, card := &nodes[(i*7)%scaleNodes], (i*5)%scaleCards
		in := store.Instance{
			UUID: fmt.Sprintf("inst-%05d", i), DeploymentID: deployments[i%scaleDeploys].ID, NodeID: node.ID, State: proto.InstRunning,
			GPUIndexes: fmt.Sprintf("[%d]", card), NNodes: 1, CreatedAt: now.Add(-2 * time.Hour),
			DesiredStateJSON: fmt.Sprintf(`{"desired_sleep":0,"memory_claim_bytes":%d}`, int64(8+i%3*4)<<30),
		}
		if i%4 == 0 { // a quarter sleep
			in.SleepLevel, in.DesiredStateJSON = 1, `{"desired_sleep":1}`
		}
		if i%11 == 0 {
			in.State = proto.InstStarting
		}
		instances[i] = in
	}
	if err := db.CreateInBatches(&instances, 500).Error; err != nil {
		t.Fatal(err)
	}
	waiting := now.Add(-time.Minute)
	if err := db.Model(&store.Instance{}).Where("uuid IN ?", []string{"inst-00004", "inst-00008", "inst-00012"}).Update("wake_waiting_since", waiting).Error; err != nil {
		t.Fatal(err)
	}

	offline := map[string]bool{}
	for i := range nodes {
		if nodes[i].State == store.NodeStateOffline {
			offline[nodes[i].UUID] = true
		}
	}
	signer, err := auth.NewTokenSigner("scale-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Store: rig.st, Bus: rig.st.Bus, AuthSigner: signer, SchedulingActivity: replayActivity{rig.st}.service(), AI: &fakeAI{available: true}, Online: func(uuid string) bool { return !offline[uuid] },
		WakeHeld: func() map[uint]int { return map[uint]int{deployments[4].ID: 2, deployments[8].ID: 5} }}
	admin, _ := rig.st.VerifyUser("admin", "password")
	session, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	rig.h = Router(deps)
	return &scaleFleet{rig: rig, a: &apiHandlers{d: deps}, deps: deps, session: session}
}

func (f *scaleFleet) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.session)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	f.rig.h.ServeHTTP(rec, req)
	return rec
}

func percentile(samples []time.Duration, q float64) time.Duration {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[min(int(float64(len(samples))*q), len(samples)-1)]
}

// The overview at the scale the scheduling center is sized for: one response
// of 1000 nodes x 8 cards and 3000 instances within a budget, answered from a
// cache between rebuilds, and rebuilt no more than once per floor however many
// events the fleet raises. The figures it logs are the ones quoted in
// docs/reference/api.md.
func TestSchedulingOverviewAtFleetScale(t *testing.T) {
	fleet := seedScaleFleet(t)
	rebuildBudget, requestBudget := time.Second, 200*time.Millisecond
	statements := countStatements(t, fleet)
	if raceEnabled {
		t.Log("race build: the times below are logged, not held to their budgets; the statement counts are")
	}

	// The first request builds the overview. The rebuild is timed on the test's
	// own handlers as the best of three, so that a host busy with other tests
	// does not fail a budget the code meets.
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rec := fleet.get("/api/v1/scheduling/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	rebuild, rebuildStatements := time.Hour, int64(0)
	for i := 0; i < 3; i++ {
		fleet.a.overview.snapshot = nil
		statements.Store(0)
		started := time.Now()
		if _, err := fleet.a.currentOverview(); err != nil {
			t.Fatal(err)
		}
		rebuild, rebuildStatements = min(rebuild, time.Since(started)), max(rebuildStatements, statements.Load())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("a client that accepts gzip got %q: %v", rec.Header().Get("Content-Encoding"), err)
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	t.Logf("rebuild + encode: %v (budget %v); %d bytes of JSON, %d gzipped; heap grew %d KiB; %d statements", rebuild, rebuildBudget, raw.Len(), rec.Body.Len(), (int64(after.HeapAlloc)-int64(before.HeapAlloc))/1024, rebuildStatements)
	if overBudget(rebuild, rebuildBudget) {
		t.Errorf("the rebuild took %v, budget %v", rebuild, rebuildBudget)
	}
	if rebuildStatements > overviewStatementBudget {
		t.Errorf("the rebuild ran %d statements, want at most %d whatever the fleet's size", rebuildStatements, overviewStatementBudget)
	}
	if raw.Len() > 1<<20 {
		t.Errorf("the overview is %d bytes of JSON for %d cards, want under 1 MiB", raw.Len(), scaleNodes*scaleCards)
	}
	var out dto.SchedulingOverview
	if err := jsonDecode(raw.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	s := out.Summary
	if s.Nodes != scaleNodes || s.Cards != scaleNodes*scaleCards || len(out.Nodes) != scaleNodes ||
		s.Awake+s.Asleep+s.Free+s.HeldOutside+s.Unhealthy+s.Offline != s.Cards ||
		s.Offline != scaleNodes/25*scaleCards || s.Asleep == 0 || s.Awake == 0 || s.Free == 0 || s.HeldOutside == 0 ||
		s.Contention.WaitingWakes != 3 || s.Contention.HeldRequests != 7 {
		t.Fatalf("summary of the scale fleet = %+v", s)
	}
	if got := fleet.a.overview.rebuilds.Load(); got != 3 {
		t.Fatalf("the test's own handlers rebuilt %d times, want the 3 timed", got)
	}

	// Cached: the p95 of repeated requests, with and without revalidation.
	etag := rec.Header().Get("ETag")
	samples := make([]time.Duration, 0, 200)
	for i := 0; i < 200; i++ {
		started := time.Now()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/scheduling/overview", nil)
		req.Header.Set("Authorization", "Bearer "+fleet.session)
		req.Header.Set("Accept-Encoding", "gzip")
		if i%2 == 1 {
			req.Header.Set("If-None-Match", etag)
		}
		cached := httptest.NewRecorder()
		fleet.rig.h.ServeHTTP(cached, req)
		samples = append(samples, time.Since(started))
		if want := map[bool]int{false: http.StatusOK, true: http.StatusNotModified}[i%2 == 1]; cached.Code != want {
			// a rebuild between two requests changes the validator only when the content changed
			if !(i%2 == 1 && cached.Code == http.StatusOK) {
				t.Fatalf("request %d = %d, want %d", i, cached.Code, want)
			}
		}
	}
	p95 := percentile(samples, 0.95)
	t.Logf("cached request p50 %v p95 %v max %v (budget p95 %v)", percentile(samples, 0.5), p95, percentile(samples, 1), requestBudget)
	if overBudget(p95, requestBudget) {
		t.Errorf("cached p95 = %v, budget %v", p95, requestBudget)
	}

	// A burst of events - every node reports status, instances change state -
	// while the page polls: the overview is rebuilt once per floor. The cache is
	// warm when the burst starts, so the first rebuild comes a floor later.
	const burst = 3 * time.Second
	handlers := fleet.a
	warm := handlers.overview.rebuilds.Load()
	deadline := time.Now().Add(burst)
	events, requests := 0, 0
	for time.Now().Before(deadline) {
		for i := 0; i < 100; i++ {
			handlers.d.Bus.PublishChange("node", fmt.Sprintf("node-%04d", events%scaleNodes), "status")
			handlers.d.Bus.PublishChange("instance", fmt.Sprintf("inst-%05d", events%scaleInstances), "running")
			events += 2
		}
		if _, err := handlers.currentOverview(); err != nil {
			t.Fatal(err)
		}
		requests++
		time.Sleep(time.Millisecond)
	}
	rebuilds := handlers.overview.rebuilds.Load() - warm
	t.Logf("burst: %d events and %d requests in %v rebuilt the overview %d times", events, requests, burst, rebuilds)
	if limit := int64(burst/overviewRefreshFloor) + 1; rebuilds > limit {
		t.Errorf("%d events rebuilt the overview %d times in %v, want at most %d (one per %v)", events, rebuilds, burst, limit, overviewRefreshFloor)
	}
	if rebuilds < 1 {
		t.Errorf("a burst of changes did not refresh the overview (%d builds)", rebuilds)
	}

	// The drawer's reads stay small however large the fleet is.
	for _, path := range []string{
		"/api/v1/scheduling/board?node=node-0500",
		"/api/v1/scheduling/timeline?node=node-0500&hours=6",
	} {
		started := time.Now()
		if got := fleet.get(path); got.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body)
		}
		took := time.Since(started)
		t.Logf("%s: %v", path, took)
		if overBudget(took, 4*requestBudget) {
			t.Errorf("%s took %v at fleet scale", path, took)
		}
	}

	sharingAtFleetScale(t, fleet, statements, rebuildBudget, requestBudget)
}

// countStatements counts the statements run on the fleet's store from now on.
func countStatements(t *testing.T, fleet *scaleFleet) *atomic.Int64 {
	t.Helper()
	var statements atomic.Int64
	db := fleet.rig.st.DB()
	count := func(*gorm.DB) { statements.Add(1) }
	for _, err := range []error{
		db.Callback().Query().After("gorm:query").Register("test:count_query", count),
		db.Callback().Row().After("gorm:row").Register("test:count_row", count),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove("test:count_query")
		_ = db.Callback().Row().Remove("test:count_row")
	})
	return &statements
}

// overBudget reports whether took exceeds budget. A race build only logs
// the times: the race detector slows the code by an amount that depends on
// the host, so its times say nothing about the code, and the plain build
// (make test, CI) holds the budgets; the statement counts hold in both.
func overBudget(took, budget time.Duration) bool {
	return !raceEnabled && took > budget
}

// sharingAtFleetScale checks the fleet's sharing stats, which read the
// scheduling activity's rollups of every shared deployment over the window: a
// rebuild, not a drawer's read.
// The work is asserted - a fixed number of statements whatever the fleet's
// size - and the time of a cold build is the best of three against the
// rebuild budget, so a host busy with other tests does not fail a budget the
// code meets; a repeat within the cache's lifetime is answered from it within
// the request budget.
func sharingAtFleetScale(t *testing.T, fleet *scaleFleet, statements *atomic.Int64, rebuildBudget, requestBudget time.Duration) {
	t.Helper()
	const path = "/api/v1/scheduling/timeline?sharing=1&window=24h"

	// Cold: the stats built on the test's own handlers, nothing cached, as
	// the best of three; every build runs a fixed set of statements.
	cold, most := time.Hour, int64(0)
	for i := 0; i < 3; i++ {
		statements.Store(0)
		started := time.Now()
		if _, err := fleet.a.sharingTimeline(time.Now().UTC(), sharingWindows[len(sharingWindows)-1]); err != nil {
			t.Fatal(err)
		}
		cold, most = min(cold, time.Since(started)), max(most, statements.Load())
	}
	t.Logf("%s cold: %v (best of 3, budget %v), %d statements", path, cold, rebuildBudget, most)
	if overBudget(cold, rebuildBudget) {
		t.Errorf("%s took %v cold, budget %v", path, cold, rebuildBudget)
	}
	if most > sharingStatementBudget {
		t.Errorf("%s ran %d statements cold, want at most %d whatever the fleet's size", path, most, sharingStatementBudget)
	}

	// Through the router: the first request builds, a repeat within the
	// cache's lifetime runs only the session's statements.
	if got := fleet.get(path); got.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", path, got.Code, got.Body)
	}
	statements.Store(0)
	started := time.Now()
	if got := fleet.get(path); got.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", path, got.Code, got.Body)
	}
	cached := time.Since(started)
	t.Logf("%s cached: %v (budget %v), %d statements", path, cached, requestBudget, statements.Load())
	if overBudget(cached, requestBudget) {
		t.Errorf("%s took %v from the cache, budget %v", path, cached, requestBudget)
	}
	if got := statements.Load(); got > sessionStatements {
		t.Errorf("a repeat within %v ran %d statements, want the session's %d at most", sharingTTL, got, sessionStatements)
	}
}

func jsonDecode(raw []byte, into any) error { return json.Unmarshal(raw, into) }
