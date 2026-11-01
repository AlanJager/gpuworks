package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/schedactivity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// replayActivity reads the scheduling activity as a server does right after
// it starts: the memory replayed from the event log, here on every call, so
// a test that writes events straight into the table sees them.
type replayActivity struct{ st *store.Store }

func (r replayActivity) service() *schedactivity.Service {
	svc := &schedactivity.Service{Tracker: schedactivity.NewTracker(), Store: r.st, Now: time.Now}
	if _, _, err := svc.Replay(time.Now()); err != nil {
		panic(err)
	}
	return svc
}

func (r replayActivity) Activity(ctx context.Context, from, to time.Time, step time.Duration, topN int) (dto.SchedulingActivity, error) {
	return r.service().Activity(ctx, from, to, step, topN)
}

func (r replayActivity) Deployments(from, to time.Time, want map[uint]bool) map[uint]store.DeploymentActivity {
	return r.service().Deployments(from, to, want)
}

func (r replayActivity) DeploymentsOnNodes(nodes []string, from, to time.Time, want map[uint]bool) map[uint]store.ResidencySwitches {
	return r.service().DeploymentsOnNodes(nodes, from, to, want)
}

// fixedActivity answers what it is given and records what it was asked.
type fixedActivity struct {
	out   dto.SchedulingActivity
	asked []string
}

func (f *fixedActivity) Activity(_ context.Context, from, to time.Time, step time.Duration, topN int) (dto.SchedulingActivity, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s %v %d", to.Sub(from), step, topN))
	return f.out, nil
}

func (f *fixedActivity) Deployments(time.Time, time.Time, map[uint]bool) map[uint]store.DeploymentActivity {
	return map[uint]store.DeploymentActivity{}
}

func (f *fixedActivity) DeploymentsOnNodes([]string, time.Time, time.Time, map[uint]bool) map[uint]store.ResidencySwitches {
	return map[uint]store.ResidencySwitches{}
}

// The activity on the wire: buckets stacked by reason, the reasons in their
// order with the range's totals, the cards that changed hands most named by
// their node.
func TestSchedulingActivityWire(t *testing.T) {
	r := newTimelineRig(t)
	other := r.node.UUID
	// A preemption (rank) and an idle yield by the shared deployment off
	// card 0, a wake that brought it back, and an unschedulable deployment.
	r.st.AddDeploymentEventCode(r.shared.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "rank", "for_deployment": r.plain.ID, "instances": []string{"inst-shared"}})
	r.st.AddDeploymentEventCode(r.shared.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "idle", "for_deployment": r.plain.ID, "instances": []string{"inst-shared"}})
	r.st.AddDeploymentEventCode(r.shared.ID, store.EventKindAutomation, "AUTO_WAKE", map[string]any{"trigger": "request", "instances": []string{"inst-shared"}})
	r.st.AddDeploymentEventCode(r.plain.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{"node": other})
	// A replica placed and one moved off a drained node: placement.
	r.st.AddDeploymentEventCode(r.plain.ID, store.EventKindAutomation, "UNIT_PLACED", map[string]any{"node": other, "replica": 1})
	r.st.AddDeploymentEventCode(r.plain.ID, store.EventKindAutomation, "NODE_DRAIN_MOVED", map[string]any{"node": other, "instance": "inst-plain"})
	rec := r.do(r.session, http.MethodGet, "/api/v1/scheduling/activity?window=1h", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("activity = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	var got dto.SchedulingActivity
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BucketSeconds != 60 || len(got.Buckets) < 60 || len(got.Buckets) > 61 || got.BeyondRetention {
		t.Fatalf("buckets = %d of %ds, beyond %v", len(got.Buckets), got.BucketSeconds, got.BeyondRetention)
	}
	if want := `"reasons":[{"key":"idle_yield","count":1},{"key":"rank_preemption","count":1},{"key":"timeslice_turn","count":0},{"key":"operator_yield","count":0},{"key":"scheduled","count":0},{"key":"placement","count":2},{"key":"failure","count":0},{"key":"queued","count":1}]`; !strings.Contains(body, want) {
		t.Fatalf("reasons on the wire:\n%s\nwant %s", body, want)
	}
	if want := fmt.Sprintf(`"top_cards":[{"node_uuid":%q,"node":"tl-node","gpu_index":0,"switches":3}]`, r.node.UUID); !strings.Contains(body, want) {
		t.Fatalf("top cards on the wire:\n%s\nwant %s", body, want)
	}
	last := got.Buckets[len(got.Buckets)-1]
	if fmt.Sprint(last.Counts) != "[1 1 0 0 0 2 0 1]" || !strings.Contains(body, `"counts":[0,0,0,0,0,0,0,0]`) {
		t.Fatalf("last bucket = %+v", last)
	}
	if !got.From.Equal(got.Buckets[0].Start) || got.To.Before(last.Start) {
		t.Fatalf("range %v..%v, buckets from %v", got.From, got.To, got.Buckets[0].Start)
	}
}

// Each quick window has its bucket; a custom range leaves the bucket to the
// service; the default is 24 hours; answers are kept for a while.
func TestSchedulingActivityWindowsAndCache(t *testing.T) {
	r := newTimelineRig(t)
	fixed := &fixedActivity{out: dto.SchedulingActivity{Reasons: []dto.ActivityReason{}, Buckets: []dto.ActivityBucket{}, TopCards: []dto.ActivityCard{}}}
	deps := Deps{Store: r.st, SchedulingActivity: fixed}
	a := &apiHandlers{d: deps}
	since := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	for _, q := range []string{"", "window=15m", "window=1h", "window=6h", "window=24h", "window=7d", "since=" + since, "window=1h"} {
		rec := callHandler(a.schedulingActivity, "/api/v1/scheduling/activity?"+q)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", q, rec.Code, rec.Body)
		}
	}
	want := []string{"24h0m0s 15m0s 20", "15m0s 1m0s 20", "1h0m0s 1m0s 20", "6h0m0s 5m0s 20", "168h0m0s 1h0m0s 20"}
	if len(fixed.asked) != 6 || strings.Join(fixed.asked[:5], "|") != strings.Join(want, "|") || !strings.HasPrefix(fixed.asked[5], "72h0m") || !strings.HasSuffix(fixed.asked[5], " 0s 20") {
		t.Fatalf("asked %q, want %q then the custom range with no bucket (the default and 24h share one answer, the second 1h is kept)", fixed.asked, want)
	}
}

func callHandler(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Every malformed request is 400 FIELD_INVALID naming the field; the route
// needs a session, answers GET only, and 404 on a server without the
// activity.
func TestSchedulingActivityRefusals(t *testing.T) {
	r := newTimelineRig(t)
	for query, field := range map[string]string{
		"window=2h":                                             "window",
		"window=1h&since=2026-10-30T00:00:00Z":                  "window",
		"until=2026-10-30T00:00:00Z":                            "since",
		"since=yesterday":                                       "since",
		"since=2026-10-30T00:00:00Z&until=x":                    "until",
		"since=2026-10-30T00:00:00Z&until=2026-10-29T00:00:00Z": "until",
		"since=2010-01-01T00:00:00Z&until=2026-01-01T00:00:00Z": "since",
	} {
		rec := r.do(r.session, http.MethodGet, "/api/v1/scheduling/activity?"+query, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) || !strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("%s = %d %s, want 400 FIELD_INVALID on %s", query, rec.Code, rec.Body, field)
		}
	}
	if rec := r.do("", http.MethodGet, "/api/v1/scheduling/activity", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	if rec := r.do(r.session, http.MethodPost, "/api/v1/scheduling/activity", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
	bare := &apiHandlers{d: Deps{Store: r.st}}
	if rec := callHandler(bare.schedulingActivity, "/api/v1/scheduling/activity"); rec.Code != http.StatusNotFound {
		t.Fatalf("without the activity = %d", rec.Code)
	}
}
