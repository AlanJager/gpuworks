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
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
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
	levels := []string{store.ServiceLevelResident, store.ServiceLevelOnline, store.ServiceLevelShared, store.ServiceLevelOffline}
	deployments := make([]store.Deployment, scaleDeploys)
	for i := range deployments {
		deployments[i] = store.Deployment{
			Name: fmt.Sprintf("dep-%03d", i), ServedName: fmt.Sprintf("dep-%03d", i), ModelID: models[i%len(models)].ID, Engine: "vllm", Sleepable: true,
			State: store.DeployActive, ServiceLevel: levels[i%len(levels)], Replicas: 1,
		}
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
	deps := Deps{Store: rig.st, Bus: rig.st.Bus, AuthSigner: signer, AI: &fakeAI{available: true}, Online: func(uuid string) bool { return !offline[uuid] },
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
	if raceEnabled {
		rebuildBudget, requestBudget = 8*time.Second, 2*time.Second
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
	rebuild := time.Hour
	for i := 0; i < 3; i++ {
		fleet.a.overview.snapshot = nil
		started := time.Now()
		if _, err := fleet.a.currentOverview(); err != nil {
			t.Fatal(err)
		}
		rebuild = min(rebuild, time.Since(started))
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("a client that accepts gzip got %q: %v", rec.Header().Get("Content-Encoding"), err)
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	t.Logf("rebuild + encode: %v (budget %v); %d bytes of JSON, %d gzipped; heap grew %d KiB", rebuild, rebuildBudget, raw.Len(), rec.Body.Len(), (int64(after.HeapAlloc)-int64(before.HeapAlloc))/1024)
	if rebuild > rebuildBudget {
		t.Errorf("the rebuild took %v, budget %v", rebuild, rebuildBudget)
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
	if p95 > requestBudget {
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
		"/api/v1/scheduling/timeline?sharing=1&hours=24",
	} {
		started := time.Now()
		if got := fleet.get(path); got.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body)
		}
		took := time.Since(started)
		t.Logf("%s: %v", path, took)
		if took > 4*requestBudget {
			t.Errorf("%s took %v at fleet scale", path, took)
		}
	}
}

func jsonDecode(raw []byte, into any) error { return json.Unmarshal(raw, into) }
