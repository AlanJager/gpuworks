package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

type timelineRig struct {
	*scheduleRig
	node        *store.Node
	t0          time.Time
	shared, res *store.Deployment
}

// newTimelineRig builds a node with two cards, a shared deployment on card 0
// and a resident one on card 1, and a past three hours old that the tests add
// events to.
func newTimelineRig(t *testing.T) *timelineRig {
	t.Helper()
	rig := newScheduleRig(t)
	node, err := rig.st.RegisterNode("tl-node", store.HashToken("token"), "tl-node")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := rig.st.DB().Create(&store.GPU{NodeID: node.ID, Ident: fmt.Sprintf("g%d", i), Index: i, Name: "RTX 8000", MemoryTotal: 48 << 30}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name, level string) *store.Deployment {
		dep, err := rig.st.CreateDeployment(presetDeployment(&store.Deployment{ModelID: 1, NodeID: node.ID, Name: name, ServedName: name, Engine: "vllm", Sleepable: true, State: store.DeployActive}, level))
		if err != nil {
			t.Fatal(err)
		}
		return dep
	}
	r := &timelineRig{scheduleRig: rig, node: node, t0: time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour), shared: mk("night-batch", yieldpolicy.TimeSlice), res: mk("always-on", yieldpolicy.Never)}
	for _, in := range []store.Instance{
		{UUID: "inst-shared", DeploymentID: r.shared.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: r.t0},
		{UUID: "inst-plain", DeploymentID: r.plain.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", CreatedAt: r.t0},
		{UUID: "inst-res", DeploymentID: r.res.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", CreatedAt: r.t0.Add(-48 * time.Hour)},
	} {
		in := in
		if err := rig.st.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *timelineRig) event(uuid string, dep uint, kind, code string, after time.Duration, data string) {
	r.t.Helper()
	if err := r.st.DB().Create(&store.InstanceEvent{InstanceUUID: uuid, DeploymentID: dep, Kind: kind, Code: code, Data: data, CreatedAt: r.t0.Add(after)}).Error; err != nil {
		r.t.Fatal(err)
	}
}

func (r *timelineRig) get(query string) (int, string) {
	rec := r.do(r.session, http.MethodGet, "/api/v1/scheduling/timeline"+query, "")
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestSchedulingTimelineWire(t *testing.T) {
	r := newTimelineRig(t)
	// shared: starts, runs, sleeps (L2) at +1h, wakes at +2h.
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_CREATED", 0, `{"state":"pending"}`)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 10*time.Minute, `{"state":"running","phase":""}`)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", time.Hour, `{"state":"running","phase":"sleep"}`)
	r.event("inst-shared", r.shared.ID, "sleep", "INSTANCE_SLEEP_RESULT", time.Hour+time.Minute, `{"result":"applied","level":2}`)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", time.Hour+time.Minute, `{"state":"running","phase":""}`)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 2*time.Hour, `{"state":"running","phase":"sleep"}`)
	r.event("inst-shared", r.shared.ID, "sleep", "INSTANCE_SLEEP_RESULT", 2*time.Hour+time.Minute, `{"result":"applied","level":0}`)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 2*time.Hour+time.Minute, `{"state":"running","phase":""}`)
	// It yielded once to the deployment sharing card 1 and was woken twice.
	r.st.AddDeploymentEventCode(r.shared.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "rank", "for_deployment": r.plain.ID, "level": 2})
	r.st.AddDeploymentEventCode(r.shared.ID, store.EventKindAutomation, "AUTO_WAKE", map[string]any{"trigger": "resume"})
	r.st.AddDeploymentEventCode(r.shared.ID, store.EventKindAutomation, "AUTO_WAKE", map[string]any{"trigger": "schedule"})
	// A neighbour on the page put to sleep for the shared deployment counts as yielded-to.
	r.st.AddDeploymentEventCode(r.plain.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "turn", "for_deployment": r.shared.ID})
	// The resident one: nothing recorded at all, so its row says awake.

	status, body := r.get("?node=" + r.node.UUID)
	if status != http.StatusOK {
		t.Fatalf("timeline = %d %s", status, body)
	}
	var got dto.SchedulingTimeline
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	f := func(d time.Duration) string { return r.t0.Add(d).Format(time.RFC3339) }
	to := got.Window.To.Format(time.RFC3339)
	from := got.Window.From.Format(time.RFC3339)
	if want := fmt.Sprintf(`"window":{"from":%q,"to":%q,"hours":6}`, from, to); !strings.Contains(body, want) {
		t.Fatalf("window\n got  %s\n want %s", body, want)
	}
	seg := func(dep uint, name, inst, state string, level int, a, b string) string {
		lv := ""
		if level > 0 {
			lv = fmt.Sprintf(`"level":%d,`, level)
		}
		return fmt.Sprintf(`{"deployment_id":%d,"deployment":%q,"instance":%q,"state":%q,%s"from":%q,"to":%q}`, dep, name, inst, state, lv, a, b)
	}
	card0 := strings.Join([]string{
		seg(r.shared.ID, "night-batch", "inst-shared", "starting", 0, f(0), f(10*time.Minute)),
		seg(r.shared.ID, "night-batch", "inst-shared", "awake", 0, f(10*time.Minute), f(time.Hour)),
		seg(r.shared.ID, "night-batch", "inst-shared", "switching", 0, f(time.Hour), f(time.Hour+time.Minute)),
		seg(r.shared.ID, "night-batch", "inst-shared", "asleep", 2, f(time.Hour+time.Minute), f(2*time.Hour)),
		seg(r.shared.ID, "night-batch", "inst-shared", "switching", 0, f(2*time.Hour), f(2*time.Hour+time.Minute)),
		seg(r.shared.ID, "night-batch", "inst-shared", "awake", 0, f(2*time.Hour+time.Minute), to),
	}, ",")
	wantNode := fmt.Sprintf(`"nodes":[{"uuid":%q,"name":"tl-node","gpus":[{"index":0,"segments":[%s],"truncated":false},{"index":1,"segments":[%s],"truncated":false}]}]`,
		r.node.UUID, card0, seg(r.res.ID, "always-on", "inst-res", "awake", 0, from, to)+","+seg(r.plain.ID, "plain", "inst-plain", "awake", 0, f(0), to))
	if !strings.Contains(body, wantNode) {
		t.Fatalf("nodes\n got  %s\n want %s", body, wantNode)
	}
	// The stats are of the shared deployment only; awake for 50 minutes before
	// the sleep and from the wake on.
	if len(got.Deployments) != 1 {
		t.Fatalf("deployments = %+v, want the shared one only", got.Deployments)
	}
	d := got.Deployments[0]
	wantAwake := int64((50*time.Minute + got.Window.To.Sub(r.t0.Add(2*time.Hour+time.Minute))).Seconds() + 0.5)
	if d.AwakeSeconds != wantAwake {
		t.Fatalf("awake seconds = %d, want %d", d.AwakeSeconds, wantAwake)
	}
	// It has existed for the 3 hours since its instance was created (its row
	// is newer, which a deployment's never is in life): the share is of
	// those, not of the 6 h window.
	present := int64(got.Window.To.Sub(r.t0).Seconds())
	if d.PresentSeconds != present {
		t.Fatalf("present seconds = %d, want %d", d.PresentSeconds, present)
	}
	if want := float64(wantAwake) / float64(present); d.AwakeShare != want {
		t.Fatalf("awake share = %v, want %v (awake over present)", d.AwakeShare, want)
	}
	// The row as it is on the wire, with the awake figures (which move with
	// the clock) checked above.
	wire := body[strings.Index(body, `"deployments":[`):]
	if want := fmt.Sprintf(`"deployments":[{"deployment_id":%d,"deployment":"night-batch","yield_policy":"timeslice","awake_seconds":%d,"awake_share":%s,"present_seconds":%d,"switches":3,"yielded":1,"yielded_to":1,"wakes":2,"held_requests":0,"cards":[{"node_id":%d,"node_uuid":%q,"node":"tl-node","gpus":[0]}]}]}`,
		r.shared.ID, wantAwake, strconv.FormatFloat(d.AwakeShare, 'g', -1, 64), present, r.node.ID, r.node.UUID); wire != want {
		t.Fatalf("stats\n got  %s\n want %s", wire, want)
	}
}

func TestSchedulingTimelineValidatesAndClips(t *testing.T) {
	r := newTimelineRig(t)
	for query, want := range map[string]int{
		"":                                    http.StatusBadRequest,
		"?hours=6":                            http.StatusBadRequest,
		"?node=" + r.node.UUID + "&hours=12":  http.StatusBadRequest,
		"?node=" + r.node.UUID + "&hours=abc": http.StatusBadRequest,
		"?node=" + r.node.UUID + "&hours=24":  http.StatusOK,
		"?node=" + r.node.UUID:                http.StatusOK,
	} {
		if status, body := r.get(query); status != want {
			t.Errorf("%q = %d %s, want %d", query, status, body, want)
		}
	}
	// A node that went since the board was read is left out, not an error.
	status, body := r.get("?node=gone&node=" + r.node.UUID)
	var got dto.SchedulingTimeline
	if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || len(got.Nodes) != 1 || got.Nodes[0].UUID != r.node.UUID {
		t.Fatalf("with an unknown node = %d %s", status, body)
	}
	// Nothing but unknown nodes: an empty timeline, bare arrays.
	status, body = r.get("?node=gone")
	if status != http.StatusOK || !strings.Contains(body, `"nodes":[],"deployments":[]`) {
		t.Fatalf("only unknown nodes = %d %s", status, body)
	}
	// 24 h clips a long-lived instance to the window.
	_, body = r.get("?node=" + r.node.UUID + "&hours=24")
	json.Unmarshal([]byte(body), &got)
	if got.Window.Hours != 24 || got.Window.To.Sub(got.Window.From) != 24*time.Hour {
		t.Fatalf("window = %+v", got.Window)
	}
	res := got.Nodes[0].GPUs[1].Segments
	if len(res) != 2 || res[0].Instance != "inst-res" || !res[0].From.Equal(got.Window.From) || !res[0].To.Equal(got.Window.To) {
		t.Fatalf("resident segments = %+v, want one covering the window, then the neighbour from its creation", res)
	}
	// Auth and method.
	if rec := r.do("", http.MethodGet, "/api/v1/scheduling/timeline?node=x", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	if rec := r.do(r.session, http.MethodPost, "/api/v1/scheduling/timeline?node=x", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestSchedulingTimelineCapsSegmentsPerCard(t *testing.T) {
	r := newTimelineRig(t)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 0, `{"state":"running","phase":""}`)
	// 250 switches a minute apart: 250 sleep phases and 250 awake stretches.
	for i := 1; i <= 250; i++ {
		phase := "sleep"
		if i%2 == 0 {
			phase = ""
		}
		r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", time.Duration(i)*20*time.Second, fmt.Sprintf(`{"state":"running","phase":%q}`, phase))
	}
	_, body := r.get("?node=" + r.node.UUID)
	var got dto.SchedulingTimeline
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	card := got.Nodes[0].GPUs[0]
	if len(card.Segments) != timelineSegmentCap || !card.Truncated {
		t.Fatalf("segments = %d truncated = %v, want %d truncated", len(card.Segments), card.Truncated, timelineSegmentCap)
	}
	if !card.Segments[len(card.Segments)-1].To.Equal(got.Window.To) {
		t.Fatal("the newest segments were not the ones kept")
	}
	if got.Nodes[0].GPUs[1].Truncated {
		t.Fatal("the other card was truncated")
	}
}

func TestBoardOccupantsCarryTheYieldPolicy(t *testing.T) {
	r := newTimelineRig(t)
	rec := r.do(r.session, http.MethodGet, "/api/v1/scheduling/board", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("board = %d %s", rec.Code, rec.Body)
	}
	var board struct {
		Nodes struct {
			Items []struct {
				GPUs []struct {
					Occupants []map[string]any `json:"occupants"`
				} `json:"gpus"`
			} `json:"items"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	levels := map[string]string{}
	for _, node := range board.Nodes.Items {
		for _, gpu := range node.GPUs {
			for _, o := range gpu.Occupants {
				levels[fmt.Sprint(o["deployment"])] = fmt.Sprint(o["yield_policy"])
			}
		}
	}
	if levels["night-batch"] != "timeslice" || levels["always-on"] != "never" {
		t.Fatalf("occupant levels = %v in %s", levels, rec.Body)
	}
}

// The wait figures come from the gateway's series: the count is the rise of
// the cumulative count (a restart counted from zero again), the quantiles
// those of each interval weighted by the requests in it.
func TestHoldWaitsReadTheGatewaysSeries(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	from := now.Add(-6 * time.Hour)
	ts := func(m int) float64 { return float64(from.Add(time.Duration(m) * time.Minute).UnixMilli()) }
	series := map[string][]dto.MonitorSeries{
		// before the window 10 had been held; 4 more at m=10, then a restart
		// and 2 at m=60, 5 at m=120.
		holdWaitMetric + "_count": {{Labels: map[string]string{"deployment": "5"}, Points: []dto.Point{{ts(-5), 10}, {ts(10), 14}, {ts(60), 2}, {ts(120), 7}}}},
		holdWaitMetric + "|0.5":   {{Points: []dto.Point{{ts(10), 1000}, {ts(60), 2000}, {ts(120), 3000}}}},
		holdWaitMetric + "|0.95":  {{Points: []dto.Point{{ts(10), 4000}, {ts(60), 8000}, {ts(120), 30000}}}},
	}
	h := &apiHandlers{d: Deps{MetricsQuery: func(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		if matchers["deployment"] == "9" {
			return nil, nil // a deployment the gateway never held a request for
		}
		key := metric
		if q := matchers["quantile"]; q != "" {
			key += "|" + q
		}
		return series[key], nil
	}}}
	got := h.holdWaits([]uint{5, 9}, from, now)
	w := got[5]
	if w.held != 11 {
		t.Fatalf("held = %d, want 4+2+5", w.held)
	}
	// Weights 4, 2, 5 of 11: the median is the interval that crosses 5.5.
	if w.p50 == nil || *w.p50 != 2000 || w.p95 == nil || *w.p95 != 30000 {
		t.Fatalf("p50 = %v p95 = %v, want 2000 and 30000", w.p50, w.p95)
	}
	if _, ok := got[9]; ok {
		t.Fatal("a deployment with no series has figures")
	}
	if none := (&apiHandlers{}).holdWaits([]uint{5}, from, now); len(none) != 0 {
		t.Fatalf("without metrics = %v", none)
	}
}

// The sharing stats say where each deployment is now: its live instances'
// cards by node, a TP replica's cards sorted, two replicas on one node merged,
// a node no longer known kept by id, and a removed instance nowhere.
func TestSharingStatsSayWhereEachDeploymentIs(t *testing.T) {
	r := newTimelineRig(t)
	other, err := r.st.RegisterNode("tl-other", store.HashToken("token-2"), "tl-other")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []store.Instance{
		{UUID: "inst-shared-tp", DeploymentID: r.shared.ID, NodeID: other.ID, State: proto.InstRunning, GPUIndexes: "[3,2]", CreatedAt: r.t0},
		{UUID: "inst-shared-2", DeploymentID: r.shared.ID, NodeID: r.node.ID, State: proto.InstRunning, GPUIndexes: "[1]", CreatedAt: r.t0},
	} {
		in := in
		if err := r.st.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	status, body := r.get("?sharing=1&hours=6")
	if status != http.StatusOK {
		t.Fatalf("sharing = %d %s", status, body)
	}
	want := fmt.Sprintf(`"cards":[{"node_id":%d,"node_uuid":%q,"node":"tl-node","gpus":[0,1]},{"node_id":%d,"node_uuid":%q,"node":"tl-other","gpus":[2,3]}]`, r.node.ID, r.node.UUID, other.ID, other.UUID)
	if !strings.Contains(body, want) {
		t.Fatalf("sharing cards\n got  %s\n want %s", body, want)
	}

	placed := placementsOf([]store.ResidencyInstance{
		{UUID: "gone", DeploymentID: 7, NodeID: 5, GPUs: []int{0}, Live: false},
		{UUID: "here", DeploymentID: 7, NodeID: 999, GPUs: []int{1, 1}, Live: true},
		{UUID: "cpu", DeploymentID: 8, NodeID: 5, Live: true},
	})
	cards, err := (&apiHandlers{d: Deps{Store: r.st}}).cardsOf(placed, []uint{7, 8, 9})
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(cards)
	if string(line) != `{"7":[{"node_id":999,"node":"","gpus":[1]}],"8":[{"node_id":5,"node":"","gpus":[]}]}` {
		t.Fatalf("cards = %s", line)
	}
}
