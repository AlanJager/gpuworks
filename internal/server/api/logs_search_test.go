package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// logsHarness wires the logs callbacks as fakes over the real router, so the
// contract is verified exactly as a client sees it: route, auth, params, and
// wire JSON.
type logsHarness struct {
	store  *store.Store
	router http.Handler
	token  string
	signer *auth.TokenSigner

	searchF    *LogsFilter
	contextReq [6]any
	window     LogsWindow
	page       LogsPage
	buckets    []LogsBucket
}

func newLogsHarness(t *testing.T) *logsHarness {
	t.Helper()
	signer, err := auth.NewTokenSigner(t.TempDir()+"-logs-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	h := &logsHarness{
		store:   st,
		page:    LogsPage{},
		buckets: []LogsBucket{},
	}
	h.router = Router(Deps{
		Store:      st,
		AuthSigner: signer,
		LogsSearch: func(f LogsFilter) (LogsPage, error) {
			captured := f
			h.searchF = &captured
			return h.page, nil
		},
		LogsContext: func(instance, node string, ts, id int64, before, after int) (LogsWindow, error) {
			h.contextReq = [6]any{instance, node, ts, id, before, after}
			return h.window, nil
		},
		LogsHistogram: func(f LogsFilter, bucketMS int64) ([]LogsBucket, error) {
			captured := f
			h.searchF = &captured
			return h.buckets, nil
		},
	})
	h.token = token
	h.signer = signer
	return h
}

func (h *logsHarness) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	code, raw := h.getRaw(t, path)
	var body map[string]any
	_ = json.Unmarshal([]byte(raw), &body)
	return code, body
}

// getRaw answers the response body exactly as written, so contract tests can
// pin the literal wire JSON (key names, key set, value shapes).
func (h *logsHarness) getRaw(t *testing.T, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestLogsSearchContract(t *testing.T) {
	h := newLogsHarness(t)
	h.page = LogsPage{
		Total:       1042,
		TotalCapped: true,
		Items: []LogEntry{{
			ID: 77, Instance: "6f9a1c2d-1", Node: "node-uuid-1", Level: "warn",
			Source: "instance", TimeMS: 1789726052103, Line: `WARNING Unknown vLLM environment variable`,
		}},
	}
	code, raw := h.getRaw(t, "/api/v1/logs/search?q=error&instance=6f9a1c2d&node=node-uuid-1&level=warn&source=instance&from=1789720000000&to=1789730000000&cursor_ts=1789726052000&cursor_id=76&limit=50&with_total=1")
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%s", code, raw)
	}
	f := h.searchF
	if f == nil {
		t.Fatal("LogsSearch was not called")
	}
	if f.Query != "error" || f.Instance != "6f9a1c2d" || f.Node != "node-uuid-1" ||
		f.Level != "warn" || f.Source != "instance" || !f.SourceSet ||
		f.FromMS != 1789720000000 || f.ToMS != 1789730000000 ||
		f.CursorTS != 1789726052000 || f.CursorID != 76 ||
		f.Limit != 50 || !f.WithTotal { // the store now does its own truncation probing
		t.Fatalf("filter parsed wrong: %+v", f)
	}
	// Literal snake_case wire JSON (docs/reference/api-conventions.md).
	want := `{"items":[{"id":77,"instance":"6f9a1c2d-1","node":"node-uuid-1","level":"warn","source":"instance","time_ms":1789726052103,"line":"WARNING Unknown vLLM environment variable"}],"total":1042,"total_capped":true}`
	if raw != want {
		t.Fatalf("search wire JSON\n got %s\nwant %s", raw, want)
	}
}

func TestLogsSearchDefaultsAndEmptyDeps(t *testing.T) {
	h := newLogsHarness(t)
	code, raw := h.getRaw(t, "/api/v1/logs/search")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if f := h.searchF; f == nil || f.Limit != 200 || f.Query != "" || f.Source != "instance" || f.SourceSet {
		t.Fatalf("defaults wrong: %+v", f)
	}
	// The fake returned a zero LogsPage (nil Items): the handler still
	// answers an empty array, never null, and omits the zero totals.
	if raw != `{"items":[]}` {
		t.Fatalf("empty page wire JSON = %s, want {\"items\":[]}", raw)
	}

	// Without any logs callbacks wired (LogsSearch == nil) the endpoint still
	// answers 200 with an empty page rather than 500.
	empty := Router(Deps{Store: h.store, AuthSigner: h.signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/search", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Fatalf("nil-deps search = %d %s", rec.Code, rec.Body.String())
	}
}

func TestLogsSearchExplicitEmptySourceMeansAll(t *testing.T) {
	h := newLogsHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/search?source=", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if f := h.searchF; f == nil || f.Source != "" || !f.SourceSet {
		t.Fatalf("explicit empty source must mean every source: %+v", f)
	}
}

func TestLogsSearchRejectsInvalidEnum(t *testing.T) {
	h := newLogsHarness(t)
	for _, path := range []string{
		"/api/v1/logs/search?level=bogus",
		"/api/v1/logs/search?source=bogus",
		"/api/v1/logs/histogram?level=bogus",
	} {
		code, body := h.get(t, path)
		if code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, code)
		}
		if body["code"] != ErrorLogsFilterInvalid {
			t.Fatalf("%s error code = %v, want %s", path, body["code"], ErrorLogsFilterInvalid)
		}
	}
}

// TestLogsSearchInstancesParam proves "instances" parses into
// LogsFilter.Instances as a trimmed, comma-split list, dropping empty
// segments from stray commas.
func TestLogsSearchInstancesParam(t *testing.T) {
	h := newLogsHarness(t)
	code, _ := h.get(t, "/api/v1/logs/search?instances=inst-a,%20inst-b%20,,inst-c")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	f := h.searchF
	if f == nil {
		t.Fatal("LogsSearch was not called")
	}
	want := []string{"inst-a", "inst-b", "inst-c"}
	if len(f.Instances) != len(want) {
		t.Fatalf("Instances = %+v, want %+v", f.Instances, want)
	}
	for i, w := range want {
		if f.Instances[i] != w {
			t.Fatalf("Instances[%d] = %q, want %q", i, f.Instances[i], w)
		}
	}
}

// TestLogsSearchInstancesParamDeduplicates proves repeated UUIDs in
// "instances" collapse to one entry, so a client sending the same instance
// three times cannot eat three slots of the logsInstancesMax cap.
func TestLogsSearchInstancesParamDeduplicates(t *testing.T) {
	h := newLogsHarness(t)
	code, _ := h.get(t, "/api/v1/logs/search?instances=a,a,a")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	f := h.searchF
	if f == nil {
		t.Fatal("LogsSearch was not called")
	}
	if len(f.Instances) != 1 || f.Instances[0] != "a" {
		t.Fatalf("Instances = %+v, want exactly one deduplicated entry [a]", f.Instances)
	}
}

// TestLogsSearchInstancesAbsentIsNil proves an unset "instances" parameter
// leaves LogsFilter.Instances nil, not an empty-but-non-nil slice - the same
// "absent means unset" contract every other LogsFilter field follows.
func TestLogsSearchInstancesAbsentIsNil(t *testing.T) {
	h := newLogsHarness(t)
	if code, _ := h.get(t, "/api/v1/logs/search"); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if f := h.searchF; f == nil || f.Instances != nil {
		t.Fatalf("Instances = %+v, want nil", f.Instances)
	}
}

// TestLogsSearchInstancesConflictWithInstance proves instance+instances set
// together is rejected with the dedicated conflict code, on both endpoints
// that accept the filter, and never reaches the LogsSearch/LogsHistogram
// callback.
func TestLogsSearchInstancesConflictWithInstance(t *testing.T) {
	h := newLogsHarness(t)
	for _, path := range []string{
		"/api/v1/logs/search?instance=inst-a&instances=inst-a,inst-b",
		"/api/v1/logs/histogram?instance=inst-a&instances=inst-a,inst-b",
	} {
		h.searchF = nil
		code, body := h.get(t, path)
		if code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, code)
		}
		if body["code"] != ErrorLogsFilterInstancesConflict {
			t.Fatalf("%s error code = %v, want %s", path, body["code"], ErrorLogsFilterInstancesConflict)
		}
		if h.searchF != nil {
			t.Fatalf("%s: LogsSearch/LogsHistogram must not be called on an invalid combo", path)
		}
	}
}

// TestLogsSearchInstancesTooMany proves an instances list beyond the cap is
// rejected with the dedicated code, and exactly the cap is accepted.
func TestLogsSearchInstancesTooMany(t *testing.T) {
	h := newLogsHarness(t)
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("inst-%d", i)
	}
	path := "/api/v1/logs/search?instances=" + strings.Join(tooMany, ",")
	code, body := h.get(t, path)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if body["code"] != ErrorLogsFilterInstancesTooMany {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorLogsFilterInstancesTooMany)
	}
	if h.searchF != nil {
		t.Fatal("LogsSearch must not be called when the instances cap is exceeded")
	}

	exactly := tooMany[:64]
	code, _ = h.get(t, "/api/v1/logs/search?instances="+strings.Join(exactly, ","))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for exactly 64 instances", code)
	}
	if h.searchF == nil || len(h.searchF.Instances) != 64 {
		t.Fatalf("Instances = %+v, want 64 entries", h.searchF)
	}
}

func TestLogsContextContract(t *testing.T) {
	h := newLogsHarness(t)
	h.window = LogsWindow{
		MatchIndex: 2,
		Items: []LogEntry{
			{ID: 75, Instance: "i", TimeMS: 10, Line: "before line"},
			{ID: 76, Instance: "i", TimeMS: 11, Line: "matched ERROR line"},
			{ID: 77, Instance: "i", TimeMS: 12, Line: "after line"},
		},
	}
	code, raw := h.getRaw(t, "/api/v1/logs/context?instance=i&ts=11&id=76&before=1&after=1")
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%s", code, raw)
	}
	if h.contextReq[0] != "i" || h.contextReq[1] != "" || h.contextReq[2] != int64(11) || h.contextReq[3] != int64(76) ||
		h.contextReq[4] != 1 || h.contextReq[5] != 1 {
		t.Fatalf("context args wrong: %+v", h.contextReq)
	}
	want := `{"items":[` +
		`{"id":75,"instance":"i","node":"","level":"","source":"","time_ms":10,"line":"before line"},` +
		`{"id":76,"instance":"i","node":"","level":"","source":"","time_ms":11,"line":"matched ERROR line"},` +
		`{"id":77,"instance":"i","node":"","level":"","source":"","time_ms":12,"line":"after line"}` +
		`],"match_index":2}`
	if raw != want {
		t.Fatalf("context wire JSON\n got %s\nwant %s", raw, want)
	}

	// Window bounds are validated server-side.
	for _, bad := range []string{"before=1001", "after=-1", "ts=0"} {
		code, _ = h.get(t, "/api/v1/logs/context?instance=i&"+bad)
		if code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", bad, code)
		}
	}
	code, body := h.get(t, "/api/v1/logs/context")
	if code != http.StatusBadRequest || body["code"] != ErrorLogsContextInvalid {
		t.Fatalf("bare context = %d %v", code, body["code"])
	}
	// The cap itself is a legitimate custom window.
	code, _ = h.get(t, "/api/v1/logs/context?instance=i&ts=11&id=76&before=1000&after=1000")
	if code != http.StatusOK {
		t.Fatalf("before=1000 status = %d, want 200", code)
	}
}

func TestLogsHistogramContract(t *testing.T) {
	h := newLogsHarness(t)
	h.buckets = []LogsBucket{
		{StartMS: 1789726020000, Counts: map[string]int64{"info": 12, "error": 3}},
		{StartMS: 1789726080000, Counts: map[string]int64{"warn": 4}},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/histogram?q=error&bucket=60000", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"items":[{"start_ms":1789726020000,"counts":{"error":3,"info":12}},{"start_ms":1789726080000,"counts":{"warn":4}}],"total":2}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("histogram wire JSON\n got %s\nwant %s", got, want)
	}
	if h.searchF == nil || h.searchF.Query != "error" {
		t.Fatalf("histogram filter not parsed: %+v", h.searchF)
	}
}

// TestLogsHistogramEmptyEnvelope pins the empty histogram as the list
// envelope with "items": [] (never null), both when the store answers no
// buckets and when no histogram callback is wired at all.
func TestLogsHistogramEmptyEnvelope(t *testing.T) {
	h := newLogsHarness(t)
	h.buckets = nil
	code, raw := h.getRaw(t, "/api/v1/logs/histogram")
	if code != http.StatusOK || raw != `{"items":[],"total":0}` {
		t.Fatalf("empty histogram = %d %s", code, raw)
	}
	unwired := &logsHarness{router: Router(Deps{Store: h.store, AuthSigner: h.signer}), token: h.token}
	code, raw = unwired.getRaw(t, "/api/v1/logs/histogram")
	if code != http.StatusOK || raw != `{"items":[],"total":0}` {
		t.Fatalf("unwired histogram = %d %s", code, raw)
	}
}
