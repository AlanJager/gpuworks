package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// overviewRig is a router over a real store whose bus and presence are wired,
// as the server wires them, and a fleet small enough to read by eye:
//
//	gpu-a (online, zone=a)  L20 0: whole card awake (qwen, resident)
//	                        L20 1: a shared slice awake (batch) and one asleep (night)
//	gpu-b (offline)         L20 0: whole card awake (qwen)
//	gpu-c (online)          H100 0: held by a process outside the platform
//	                        H100 1: only a sleeper's residue (night)
//	                        H100 2: nothing
type overviewRig struct {
	*scheduleRig
	deps               Deps
	handlers           *apiHandlers // a second set of handlers over the same store, for the cache
	offline            map[string]bool
	held               map[uint]int
	qwen, batch, night *store.Deployment
}

const gib = int64(1 << 30)

func newOverviewRig(t *testing.T) *overviewRig {
	t.Helper()
	base := newScheduleRig(t)
	rig := &overviewRig{scheduleRig: base, offline: map[string]bool{"gpu-b": true}, held: map[uint]int{}}
	deps := Deps{
		Store: base.st, Bus: base.st.Bus, AuthSigner: nil,
		Online:   func(uuid string) bool { return !rig.offline[uuid] },
		WakeHeld: func() map[uint]int { return rig.held },
	}
	signer, err := auth.NewTokenSigner("overview-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deps.AuthSigner, deps.AI = signer, &fakeAI{available: true}
	admin, _ := base.st.VerifyUser("admin", "password")
	session, err := managementSessionTestToken(t, base.st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	rig.deps, rig.handlers = deps, &apiHandlers{d: deps}
	base.h, base.session = Router(deps), session

	node := func(uuid, labels string, cards ...store.GPU) *store.Node {
		n, err := base.st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if labels != "" {
			if _, err := base.st.SetNodeLabels(uuid, map[string]string{"zone": labels}); err != nil {
				t.Fatal(err)
			}
		}
		for i := range cards {
			cards[i].NodeID, cards[i].Ident = n.ID, fmt.Sprintf("%s-%d", uuid, cards[i].Index)
			if err := base.st.DB().Create(&cards[i]).Error; err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	l20 := func(i int) store.GPU { return store.GPU{Index: i, Name: "L20", MemoryTotal: 48 * gib} }
	a := node("gpu-a", "a", l20(0), l20(1))
	b := node("gpu-b", "", l20(0))
	held := store.GPU{Index: 0, Name: "H100", MemoryTotal: 80 * gib, ForeignProcesses: `[{"pid":9,"name":"python"}]`}
	c := node("gpu-c", "", held, store.GPU{Index: 1, Name: "H100", MemoryTotal: 80 * gib}, store.GPU{Index: 2, Name: "H100", MemoryTotal: 80 * gib})

	mk := func(name, level string) *store.Deployment {
		dep, err := base.st.CreateDeployment(&store.Deployment{ModelID: 1, NodeID: a.ID, Name: name, ServedName: name, Engine: "vllm", Sleepable: true, State: store.DeployActive, ServiceLevel: level})
		if err != nil {
			t.Fatal(err)
		}
		return dep
	}
	rig.qwen, rig.batch, rig.night = mk("qwen", store.ServiceLevelResident), mk("batch", store.ServiceLevelShared), mk("night", store.ServiceLevelOnline)
	spec := func(desiredSleep int, claim int64) string {
		return fmt.Sprintf(`{"desired_sleep":%d,"memory_claim_bytes":%d}`, desiredSleep, claim)
	}
	for _, in := range []store.Instance{
		{UUID: "qwen-a", DeploymentID: rig.qwen.ID, NodeID: a.ID, State: proto.InstRunning, GPUIndexes: "[0]"},
		{UUID: "batch-a", DeploymentID: rig.batch.ID, NodeID: a.ID, State: proto.InstRunning, GPUIndexes: "[1]", DesiredStateJSON: spec(0, 12*gib)},
		{UUID: "night-a", DeploymentID: rig.night.ID, NodeID: a.ID, State: proto.InstRunning, GPUIndexes: "[1]", SleepLevel: 1, SleepResidueBytes: 2 * gib, DesiredStateJSON: spec(1, 24*gib)},
		{UUID: "qwen-b", DeploymentID: rig.qwen.ID, NodeID: b.ID, State: proto.InstRunning, GPUIndexes: "[0]"},
		{UUID: "night-c", DeploymentID: rig.night.ID, NodeID: c.ID, State: proto.InstRunning, GPUIndexes: "[1]", SleepLevel: 2, SleepResidueBytes: 4 * gib, DesiredStateJSON: spec(2, 0)},
	} {
		in := in
		in.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
		if err := base.st.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

func (r *overviewRig) overview(t *testing.T) (dto.SchedulingOverview, *httptest.ResponseRecorder) {
	t.Helper()
	rec := r.do(r.session, http.MethodGet, "/api/v1/scheduling/overview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body)
	}
	var out dto.SchedulingOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("overview is not the contract: %v\n%s", err, rec.Body)
	}
	return out, rec
}

// The whole contract on the wire for the small fleet: ordering of nodes by
// name, the model and deployment dictionaries in order of first appearance,
// each card as its array, and the summary that adds up.
func TestSchedulingOverviewWire(t *testing.T) {
	rig := newOverviewRig(t)
	if err := rig.st.MarkWakeWaiting(rig.night.ID, []string{"night-a"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rig.held[rig.night.ID] = 3
	if err := rig.st.DB().Create(&store.InstanceEvent{InstanceUUID: "batch-a", DeploymentID: rig.batch.ID, Kind: store.EventKindAutomation, Code: "YIELDED_FOR_WAKE", Data: `{"deployment":"qwen"}`, CreatedAt: time.Now().UTC().Add(-10 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	_, rec := rig.overview(t)
	want := fmt.Sprintf(`{"summary":{"nodes":3,"online_nodes":2,"cards":6,"awake":2,"asleep":1,"free":1,"held_outside":1,"unhealthy":0,"offline":1,`+
		`"by_model":[{"model":"L20","cards":3,"awake":2,"asleep":0,"free":0,"held_outside":0,"unhealthy":0,"offline":1},`+
		`{"model":"H100","cards":3,"awake":0,"asleep":1,"free":1,"held_outside":1,"unhealthy":0,"offline":0}],`+
		`"contention":{"waiting_wakes":1,"held_requests":3,"yields_last_hour":1}},`+
		`"models":["L20","H100"],`+
		`"deployments":[{"id":%d,"name":"qwen","service_level":"resident"},{"id":%d,"name":"batch","service_level":"shared"},{"id":%d,"name":"night","service_level":"online"}],`+
		`"nodes":[`+
		`{"uuid":"gpu-a","name":"gpu-a","online":true,"labels":{"zone":"a"},"contention":%d,"cards":[[0,0,49152,100,0,0,[0]],[1,0,49152,25,4,0,[2,5]]]},`+
		`{"uuid":"gpu-b","name":"gpu-b","online":false,"cards":[[0,0,49152,100,0,0,[0]]]},`+
		`{"uuid":"gpu-c","name":"gpu-c","online":true,"contention":3,"cards":[[0,1,81920,0,0,1],[1,1,81920,0,5,0,[5]],[2,1,81920,0,0,0]]}]}`,
		rig.qwen.ID, rig.batch.ID, rig.night.ID, 1+3+1)
	// gpu-a's contention: 1 sleeper waiting to wake, 3 held requests for night
	// (asleep there) and 1 yield of batch (on it); gpu-c's: the 3 held
	// requests for night, which sleeps there too.
	// Deployment positions: qwen 0 (awake: 0), batch 1 (awake: 2), night 2
	// (asleep: 5).
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("overview wire =\n%s\nwant\n%s", got, want)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("ETag") == "" {
		t.Fatalf("headers = %v", rec.Header())
	}
}

// The summary's buckets are disjoint and add up to the cards.
func TestSchedulingOverviewSummaryAddsUp(t *testing.T) {
	rig := newOverviewRig(t)
	out, _ := rig.overview(t)
	s := out.Summary
	if sum := s.Awake + s.Asleep + s.Free + s.HeldOutside + s.Unhealthy + s.Offline; sum != s.Cards || s.Cards != 6 {
		t.Fatalf("summary %+v: buckets add to %d, cards %d", s, sum, s.Cards)
	}
	byModel := 0
	for _, m := range s.ByModel {
		if m.Awake+m.Asleep+m.Free+m.HeldOutside+m.Unhealthy+m.Offline != m.Cards {
			t.Errorf("model %+v does not add up", m)
		}
		byModel += m.Cards
	}
	if byModel != s.Cards {
		t.Fatalf("models count %d cards of %d", byModel, s.Cards)
	}
	// A card on an offline node is offline whatever it holds, and one held
	// outside the platform is held whatever it also carries.
	rig.offline["gpu-c"] = true
	rig.h = Router(rig.deps) // a fresh cache
	out, _ = rig.overview(t)
	if out.Summary.Offline != 4 || out.Summary.HeldOutside != 0 || out.Summary.Free != 0 {
		t.Fatalf("summary with gpu-c offline = %+v", out.Summary)
	}
}

func TestSchedulingOverviewEndpointContract(t *testing.T) {
	rig := newOverviewRig(t)
	const path = "/api/v1/scheduling/overview"
	if rec := rig.do("", http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := rig.do(rig.session, method, path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want 405", method, rec.Code)
		}
	}
	if rec := rig.do(rig.mint("read"), http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("a read token = %d %s", rec.Code, rec.Body)
	}
	// A poll that finds nothing changed is answered with a 304 and no body.
	_, rec := rig.overview(t)
	etag := rec.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+rig.session)
	req.Header.Set("If-None-Match", etag)
	cached := httptest.NewRecorder()
	rig.h.ServeHTTP(cached, req)
	if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag {
		t.Fatalf("revalidation = %d body %q etag %q", cached.Code, cached.Body, cached.Header().Get("ETag"))
	}
	req.Header.Set("If-None-Match", `"other"`)
	changed := httptest.NewRecorder()
	rig.h.ServeHTTP(changed, req)
	if changed.Code != http.StatusOK || changed.Body.Len() == 0 {
		t.Fatalf("a stale validator = %d", changed.Code)
	}
}

func TestSchedulingOverviewOfAnEmptyFleet(t *testing.T) {
	rig := newScheduleRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/overview", "")
	want := `{"summary":{"nodes":0,"online_nodes":0,"cards":0,"awake":0,"asleep":0,"free":0,"held_outside":0,"unhealthy":0,"offline":0,"by_model":[],"contention":{"waiting_wakes":0,"held_requests":0,"yields_last_hour":0}},"models":[],"deployments":[],"nodes":[]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("empty fleet = %d %s", rec.Code, rec.Body)
	}
}

// The cache: one build serves every request until the bus announces a change
// and the floor has passed; a burst of changes is one rebuild; and a quiet
// fleet is rebuilt when the snapshot is old.
func TestOverviewCacheIsVersionedAndThrottled(t *testing.T) {
	rig := newOverviewRig(t)
	a := rig.handlers
	build := func() int64 {
		t.Helper()
		if _, err := a.currentOverview(); err != nil {
			t.Fatal(err)
		}
		return a.overview.rebuilds.Load()
	}
	age := func(by time.Duration) { a.overview.snapshot.at = a.overview.snapshot.at.Add(-by) }
	if got := build(); got != 1 {
		t.Fatalf("first build = %d", got)
	}
	for i := 0; i < 50; i++ {
		build()
	}
	if got := a.overview.rebuilds.Load(); got != 1 {
		t.Fatalf("50 requests with no change rebuilt %d times", got)
	}

	// A change inside the floor is not seen yet; a burst of them is still one rebuild after it.
	for i := 0; i < 500; i++ {
		a.d.Bus.PublishChange("node", fmt.Sprintf("n%d", i%7), "status")
		a.d.Bus.PublishChange("instance", "i", "running")
	}
	if got := build(); got != 1 {
		t.Fatalf("a burst inside the floor rebuilt %d times, want the one snapshot kept", got)
	}
	age(overviewRefreshFloor)
	if got := build(); got != 2 {
		t.Fatalf("after the floor the burst rebuilt %d times in all, want 2", got)
	}
	for i := 0; i < 20; i++ {
		build()
	}
	if got := a.overview.rebuilds.Load(); got != 2 {
		t.Fatalf("requests after the rebuild rebuilt %d times in all", got)
	}

	// Progress events and other topics are not changes of the overview.
	a.d.Bus.PublishChange("instance", "i", "progress")
	a.d.Bus.PublishChange("network", "x", "changed")
	age(overviewRefreshFloor)
	if got := build(); got != 2 {
		t.Fatalf("progress and unrelated topics rebuilt (%d)", got)
	}

	// A change outlasts the floor but not a rebuild; with no change at all the
	// snapshot is rebuilt once it is old.
	age(overviewMaxAge)
	if got := build(); got != 3 {
		t.Fatalf("an old snapshot was not rebuilt (%d)", got)
	}
	// The rebuild sees what changed.
	rig.held[rig.night.ID] = 9
	a.d.Bus.PublishChange("deployment", "1", "updated")
	age(overviewRefreshFloor)
	snap, err := a.currentOverview()
	if err != nil || !strings.Contains(string(snap.body), `"held_requests":9`) {
		t.Fatalf("the rebuild did not read the held requests: %v %s", err, snap.body)
	}
}

// Concurrent requests behind a stale snapshot share one rebuild.
func TestOverviewCacheRebuildsOnceForConcurrentRequests(t *testing.T) {
	rig := newOverviewRig(t)
	a := rig.handlers
	if _, err := a.currentOverview(); err != nil {
		t.Fatal(err)
	}
	a.overview.snapshot.at = a.overview.snapshot.at.Add(-overviewMaxAge)
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := a.currentOverview(); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
	if got := a.overview.rebuilds.Load(); got != 2 {
		t.Fatalf("16 concurrent requests behind an old snapshot rebuilt %d times in all, want 2", got)
	}
}

func TestSchedulingBoardOfOneNode(t *testing.T) {
	rig := newOverviewRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board?node=gpu-a&page=7&q=nothing", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("board of a node = %d %s", rec.Code, rec.Body)
	}
	var board dto.SchedulingBoard
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	if board.Nodes.Total != 1 || len(board.Nodes.Items) != 1 || board.Nodes.Items[0].UUID != "gpu-a" || !board.Nodes.Items[0].Online {
		t.Fatalf("nodes = %+v", board.Nodes)
	}
	cards := board.Nodes.Items[0].GPUs
	if len(cards) != 2 || len(cards[1].Occupants) != 2 || cards[1].Occupants[1].State != "asleep" || cards[1].Occupants[1].ServiceLevel != "online" {
		t.Fatalf("cards = %+v", cards)
	}
	for _, list := range []string{`"pending":[]`, `"waiting_wakes":[]`, `"upcoming":[]`, `"recent":[]`} {
		if !strings.Contains(rec.Body.String(), list) {
			t.Errorf("a node board lacks %s: %s", list, rec.Body)
		}
	}
	if rec = rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board?node=gpu-b", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"online":false`) {
		t.Fatalf("an offline node = %d %s", rec.Code, rec.Body)
	}
	if rec = rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board?node=nobody", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown node = %d %s", rec.Code, rec.Body)
	}
}

// The sharing stats cover the deployments that share GPUs across the fleet
// and none of the resident ones, with no node lanes.
func TestSchedulingTimelineSharingStats(t *testing.T) {
	rig := newOverviewRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/timeline?sharing=1&hours=24", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sharing = %d %s", rec.Code, rec.Body)
	}
	var got dto.SchedulingTimeline
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, d := range got.Deployments {
		names[d.Deployment] = d.ServiceLevel
	}
	if len(got.Nodes) != 0 || len(names) != 2 || names["batch"] != "shared" || names["night"] != "online" || got.Window.Hours != 24 {
		t.Fatalf("sharing stats = %+v", got)
	}
	for _, bad := range []string{"sharing=2", "sharing=1&node=gpu-a", "sharing=0"} {
		if rec = rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/timeline?"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
	// Without sharing a node is still required.
	if rec = rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/timeline", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("no node and no sharing = %d", rec.Code)
	}
}

// The fleet's sharing stats are read once per window and kept for a while.
func TestSharingStatsAreKeptPerWindow(t *testing.T) {
	rig := newOverviewRig(t)
	a := rig.handlers
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if _, err := a.cachedSharingTimeline(now, 6); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.cachedSharingTimeline(now, 24); err != nil {
		t.Fatal(err)
	}
	if got := a.overview.sharing.reads.Load(); got != 2 {
		t.Fatalf("6 requests over two windows read the log %d times, want 2", got)
	}
	entry := a.overview.sharing.byHours[6]
	entry.at = entry.at.Add(-sharingTTL)
	a.overview.sharing.byHours[6] = entry
	if _, err := a.cachedSharingTimeline(now, 6); err != nil || a.overview.sharing.reads.Load() != 3 {
		t.Fatalf("an old entry was not read again: %v", err)
	}
}

// queues=1 answers the fleet-wide lists with no node in them.
func TestSchedulingBoardQueuesOnly(t *testing.T) {
	rig := newOverviewRig(t)
	if err := rig.st.MarkWakeWaiting(rig.night.ID, []string{"night-a"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rig.st.AddDeploymentEventCode(rig.batch.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{"scheduler": map[string]any{"code": "pool_empty"}})
	// A refusal is pending only while a replica is unplaced: ask for one
	// more replica than the instances fill.
	if err := rig.st.DB().Model(&store.Deployment{}).Where("id = ?", rig.batch.ID).Update("replicas", 9).Error; err != nil {
		t.Fatal(err)
	}
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board?queues=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("queues = %d %s", rec.Code, rec.Body)
	}
	var board dto.SchedulingBoard
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	if len(board.Nodes.Items) != 0 || board.Nodes.Total != 0 {
		t.Fatalf("queues carried nodes: %+v", board.Nodes)
	}
	if len(board.WaitingWakes) != 1 || board.WaitingWakes[0].Deployment != "night" || len(board.Pending) != 1 || board.Pending[0].Deployment != "batch" {
		t.Fatalf("queues = pending %+v waiting %+v", board.Pending, board.WaitingWakes)
	}
}
