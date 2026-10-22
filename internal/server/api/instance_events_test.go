package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type eventsHarness struct {
	store    *store.Store
	router   http.Handler
	token    string
	instance *store.Instance
}

func newEventsHarness(t *testing.T, dsn string) *eventsHarness {
	t.Helper()
	st, err := store.Open("file:"+dsn+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("events-node", store.HashToken("events-token"), "events-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "events-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner(dsn+"-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return &eventsHarness{store: st, router: Router(Deps{AuthSigner: signer, Store: st}), token: token, instance: inst}
}

func (h *eventsHarness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *eventsHarness) events(t *testing.T, path string) []dto.InstanceEvent {
	t.Helper()
	rec := h.get(t, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body = %s", path, rec.Code, rec.Body.String())
	}
	var events []dto.InstanceEvent
	if err := unmarshalCursorPage(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("GET %s body = %s: %v", path, rec.Body.String(), err)
	}
	return events
}

func TestInstanceEventsRequireAuthentication(t *testing.T) {
	h := newEventsHarness(t, "api_events_auth")
	for _, path := range []string{
		"/api/v1/instances/events-instance/events",
		"/api/v1/deployments/1/events",
	} {
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s status = %d", path, rec.Code)
		}
	}
}

func TestInstanceEventsReturnNewestFirstAndFolded(t *testing.T) {
	h := newEventsHarness(t, "api_events_folded")
	for _, step := range []struct {
		state, phase string
		samples      int
	}{
		{proto.InstPulling, "image_pull", 40},
		{proto.InstStarting, "container_start", 25},
		{proto.InstRunning, "", 1},
	} {
		for i := 0; i < step.samples; i++ {
			if err := h.store.UpdateInstanceStatus(proto.InstanceStatus{
				UUID: h.instance.UUID, State: step.state, Phase: step.phase,
				Message: fmt.Sprintf("%s sample %d", step.phase, i), BytesDone: int64(i),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	events := h.events(t, "/api/v1/instances/events-instance/events")
	if len(events) != 4 {
		t.Fatalf("returned %d entries for 66 reports, want 4 milestones: %+v", len(events), events)
	}
	if events[0].Params["state"] != proto.InstRunning {
		t.Fatalf("first entry = %+v, want the newest milestone", events[0])
	}
	if events[len(events)-1].Code != "INSTANCE_CREATED" {
		t.Fatalf("last entry = %+v, want the oldest milestone", events[len(events)-1])
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID >= events[i-1].ID {
			t.Fatalf("entries are not newest-first: %d then %d", events[i-1].ID, events[i].ID)
		}
	}

	// The deployment timeline is the same history reached the other way.
	deploymentEvents := h.events(t, "/api/v1/deployments/"+strconv.FormatUint(uint64(h.instance.DeploymentID), 10)+"/events")
	if len(deploymentEvents) != len(events) {
		t.Fatalf("deployment timeline = %d entries, instance timeline = %d", len(deploymentEvents), len(events))
	}
}

func TestInstanceEventsPageBackwardsWithTheCursor(t *testing.T) {
	h := newEventsHarness(t, "api_events_cursor")
	for i := 0; i < 8; i++ {
		if err := h.store.UpdateInstanceStatus(proto.InstanceStatus{
			UUID: h.instance.UUID, State: proto.InstPulling, Phase: fmt.Sprintf("phase_%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := h.events(t, "/api/v1/instances/events-instance/events?limit=3")
	if len(first) != 3 {
		t.Fatalf("first page = %d entries, want 3", len(first))
	}
	second := h.events(t, fmt.Sprintf("/api/v1/instances/events-instance/events?limit=3&before=%d", first[len(first)-1].ID))
	if len(second) != 3 {
		t.Fatalf("second page = %d entries, want 3", len(second))
	}
	for _, entry := range second {
		for _, earlier := range first {
			if entry.ID == earlier.ID {
				t.Fatalf("entry %d appears on both pages", entry.ID)
			}
		}
		if entry.ID >= first[len(first)-1].ID {
			t.Fatalf("entry %d is not older than the cursor %d", entry.ID, first[len(first)-1].ID)
		}
	}
	// Nine entries exist (eight phases plus creation); the third page holds the
	// remainder and the fourth is empty rather than an error.
	third := h.events(t, fmt.Sprintf("/api/v1/instances/events-instance/events?limit=3&before=%d", second[len(second)-1].ID))
	if len(third) != 3 {
		t.Fatalf("third page = %d entries, want the remaining 3", len(third))
	}
	fourth := h.events(t, fmt.Sprintf("/api/v1/instances/events-instance/events?limit=3&before=%d", third[len(third)-1].ID))
	if len(fourth) != 0 {
		t.Fatalf("page past the oldest entry = %d entries, want none", len(fourth))
	}
}

func TestInstanceEventsRejectInvalidPaging(t *testing.T) {
	h := newEventsHarness(t, "api_events_invalid")
	for _, tc := range []struct{ query, code string }{
		{"?before=abc", "EVENT_CURSOR_INVALID"},
		{"?before=-1", "EVENT_CURSOR_INVALID"},
		{"?before=0", "EVENT_CURSOR_INVALID"},
		{"?limit=abc", "EVENT_LIMIT_INVALID"},
		{"?limit=0", "EVENT_LIMIT_INVALID"},
		{"?limit=-3", "EVENT_LIMIT_INVALID"},
	} {
		for _, base := range []string{"/api/v1/instances/events-instance/events", "/api/v1/deployments/1/events"} {
			rec := h.get(t, base+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %s%s status = %d, want 400", base, tc.query, rec.Code)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body = %s: %v", rec.Body.String(), err)
			}
			if body.Code != tc.code {
				t.Fatalf("GET %s%s code = %q, want %q", base, tc.query, body.Code, tc.code)
			}
		}
	}
	// An oversized limit is clamped, not rejected: it is a request for "all of
	// it", which the server is entitled to bound.
	if rec := h.get(t, "/api/v1/instances/events-instance/events?limit=100000"); rec.Code != http.StatusOK {
		t.Fatalf("oversized limit status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := h.get(t, "/api/v1/instances/does-not-exist/events"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown instance status = %d, want 404", rec.Code)
	}
	if rec := h.get(t, "/api/v1/deployments/0/events"); rec.Code != http.StatusBadRequest {
		t.Fatalf("deployment id 0 status = %d, want 400", rec.Code)
	}
}

// The wire shape is checked literally: an id to page with, the code and params
// a client renders from, and no duplicated raw payload on a coded row.
func TestInstanceEventWireJSON(t *testing.T) {
	created := time.Date(2026, 9, 17, 10, 30, 0, 0, time.UTC)
	coded := instanceEventDTOs([]store.InstanceEvent{{
		ID: 42, Kind: "state", Code: "INSTANCE_STATUS_UPDATED",
		Data:      `{"state":"starting","phase":"container_start","detail":"starting container","prev_phase":"image_pull","prev_bytes_done":1024}`,
		CreatedAt: created,
	}})
	encoded, err := json.Marshal(coded)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":42,"kind":"state","code":"INSTANCE_STATUS_UPDATED","params":{"detail":"starting container","phase":"container_start","prev_bytes_done":1024,"prev_phase":"image_pull","state":"starting"},"created_at":"2026-09-17T10:30:00Z"}]`
	if string(encoded) != want {
		t.Fatalf("coded event JSON =\n%s\nwant\n%s", encoded, want)
	}

	legacy := instanceEventDTOs([]store.InstanceEvent{{
		ID: 7, Kind: "template_feedback", Message: "template review", Data: `{"queue":"pending"}`, CreatedAt: created,
	}})
	encodedLegacy, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	wantLegacy := `[{"id":7,"kind":"template_feedback","message":"template review","data":"{\"queue\":\"pending\"}","created_at":"2026-09-17T10:30:00Z"}]`
	if string(encodedLegacy) != wantLegacy {
		t.Fatalf("code-less event JSON =\n%s\nwant\n%s", encodedLegacy, wantLegacy)
	}

	var roundTripped []dto.InstanceEvent
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if len(roundTripped) != 1 || roundTripped[0].ID != 42 || roundTripped[0].Params["prev_phase"] != "image_pull" {
		t.Fatalf("round trip = %+v", roundTripped)
	}
}
