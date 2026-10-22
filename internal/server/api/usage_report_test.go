package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// fakeUsageReport is the program as the app wires it, without a scheduler.
type fakeUsageReport struct {
	mu       sync.Mutex
	enabled  bool
	readOnly bool
	saves    []bool
	counters usagereport.Counters
	preview  []byte
	err      error
}

func (f *fakeUsageReport) Counters() *usagereport.Counters { return &f.counters }
func (f *fakeUsageReport) Status() dto.UsageReportStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return dto.UsageReportStatus{Enabled: f.enabled, Editable: !f.readOnly, LastDay: "2026-10-21"}
}
func (f *fakeUsageReport) SetEnabled(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readOnly {
		return &ServerSettingsError{Code: ErrorUsageReportReadOnly, Fallback: "the server was started without server.yaml"}
	}
	f.enabled = on
	f.saves = append(f.saves, on)
	return nil
}
func (f *fakeUsageReport) Preview(context.Context) ([]byte, error) { return f.preview, f.err }

type fakeNotices struct {
	list      dto.NoticeList
	reads     map[uint][]string
	err       error
	refreshes int
}

func (f *fakeNotices) Refresh() { f.refreshes++ }

func (f *fakeNotices) List(userID uint) (dto.NoticeList, error) {
	out := f.list
	out.Notices = nil
	out.Unread = 0
	for _, n := range f.list.Notices {
		for _, id := range f.reads[userID] {
			if id == n.ID {
				n.Read = true
			}
		}
		if !n.Read {
			out.Unread++
		}
		out.Notices = append(out.Notices, n)
	}
	return out, f.err
}

func (f *fakeNotices) MarkRead(userID uint, ids []string) error {
	if f.err != nil {
		return f.err
	}
	if f.reads == nil {
		f.reads = map[uint][]string{}
	}
	f.reads[userID] = append(f.reads[userID], ids...)
	return nil
}

func usageRun(t *testing.T, usage *fakeUsageReport, notices *fakeNotices) *imageManifestRun {
	t.Helper()
	run := newImageManifestRun(t)
	run.deps.UsageReport = usage
	if notices != nil {
		run.deps.Notices = notices
	}
	run.router = Router(run.deps)
	return run
}

func TestUsageReportRoutes(t *testing.T) {
	usage := &fakeUsageReport{enabled: true, preview: []byte(`{"schema":1,"install_id":"abc"}`)}
	run := usageRun(t, usage, nil)

	rec := run.do(http.MethodGet, "/api/v1/usage-report", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":true,"editable":true,"last_day":"2026-10-21"}` {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	rec = run.do(http.MethodPut, "/api/v1/usage-report/enabled", "application/json", `{"enabled":false}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) || len(usage.saves) != 1 || usage.saves[0] {
		t.Fatalf("switch off = %d %s, saves %v", rec.Code, rec.Body.String(), usage.saves)
	}
	for _, body := range []string{`{}`, `{"enabled":"no"}`, `nope`, `{"enabled":null}`} {
		rec := run.do(http.MethodPut, "/api/v1/usage-report/enabled", "application/json", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorUsageReportInvalid+`"`) {
			t.Fatalf("PUT %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(usage.saves) != 1 {
		t.Fatalf("rejected requests were saved: %v", usage.saves)
	}
	// The preview is the report's bytes, verbatim.
	rec = run.do(http.MethodGet, "/api/v1/usage-report/preview", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"schema":1,"install_id":"abc"}` ||
		rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	usage.err = errors.New("database is locked")
	if rec := run.do(http.MethodGet, "/api/v1/usage-report/preview", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed preview = %d %s", rec.Code, rec.Body.String())
	}

	// Without server.yaml there is nowhere to keep the choice.
	usage.readOnly = true
	rec = run.do(http.MethodPut, "/api/v1/usage-report/enabled", "application/json", `{"enabled":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorUsageReportReadOnly+`"`) {
		t.Fatalf("read-only switch = %d %s", rec.Code, rec.Body.String())
	}

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/usage-report"}, {http.MethodPut, "/api/v1/usage-report/enabled"},
		{http.MethodGet, "/api/v1/usage-report/preview"},
	} {
		rec := httptest.NewRecorder()
		run.router.ServeHTTP(rec, httptest.NewRequest(req.method, req.path, strings.NewReader(`{"enabled":true}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", req.method, req.path, rec.Code)
		}
	}
	if rec := run.do(http.MethodPost, "/api/v1/usage-report", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
}

func TestUsageReportRoutesWithoutTheProgram(t *testing.T) {
	run := newImageManifestRun(t)
	if rec := run.do(http.MethodGet, "/api/v1/usage-report", "", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":false,"editable":false}` {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if rec := run.do(http.MethodGet, "/api/v1/usage-report/preview", "", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), ErrorUsageReportUnavailable) {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body.String())
	}
	if rec := run.do(http.MethodPut, "/api/v1/usage-report/enabled", "application/json", `{"enabled":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("switch = %d %s", rec.Code, rec.Body.String())
	}
	rec := run.do(http.MethodGet, "/api/v1/notices", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"notices":[],"unread":0}` {
		t.Fatalf("notices = %d %s", rec.Code, rec.Body.String())
	}
}

func TestNoticeRoutes(t *testing.T) {
	published := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	notices := &fakeNotices{list: dto.NoticeList{Notices: []dto.Notice{
		{ID: "cve-2026-0001", Severity: "critical", Kind: "security", Title: map[string]string{"en": "CVE"}, URL: "https://gpuworks.ai/s", PublishedAt: published},
		{ID: "meetup-1", Severity: "info", Kind: "event", Title: map[string]string{"en": "Meetup"}, PublishedAt: published},
	}}}
	run := usageRun(t, &fakeUsageReport{enabled: true}, notices)

	rec := run.do(http.MethodGet, "/api/v1/notices", "", "")
	want := `{"notices":[{"id":"cve-2026-0001","severity":"critical","kind":"security","title":{"en":"CVE"},"url":"https://gpuworks.ai/s","published_at":"2026-10-22T01:00:00Z","read":false},` +
		`{"id":"meetup-1","severity":"info","kind":"event","title":{"en":"Meetup"},"published_at":"2026-10-22T01:00:00Z","read":false}],"unread":2}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("notices = %d\n%s\nwant\n%s", rec.Code, rec.Body.String(), want)
	}
	if rec := run.do(http.MethodPost, "/api/v1/notices/read", "application/json", `{"ids":["cve-2026-0001"]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("read = %d %s", rec.Code, rec.Body.String())
	}
	var list dto.NoticeList
	rec = run.do(http.MethodGet, "/api/v1/notices", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Unread != 1 || !list.Notices[0].Read {
		t.Fatalf("after read = %s", rec.Body.String())
	}
	for user := range notices.reads {
		if user == 0 {
			t.Fatal("read marks were kept without the caller's user")
		}
	}
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"ids":[""]}`, `{"ids":["` + strings.Repeat("x", 65) + `"]}`, `[`,
		`{"ids":[` + strings.TrimSuffix(strings.Repeat(`"a",`, maxNoticesRead+1), ",") + `]}`} {
		rec := run.do(http.MethodPost, "/api/v1/notices/read", "application/json", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorNoticeIDsInvalid) {
			t.Fatalf("read %.40s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	notices.err = errors.New("disk full")
	if rec := run.do(http.MethodPost, "/api/v1/notices/read", "application/json", `{"ids":["a"]}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed read = %d", rec.Code)
	}
	for _, req := range []struct{ method, path string }{{http.MethodGet, "/api/v1/notices"}, {http.MethodPost, "/api/v1/notices/read"}} {
		rec := httptest.NewRecorder()
		run.router.ServeHTTP(rec, httptest.NewRequest(req.method, req.path, strings.NewReader(`{"ids":["a"]}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", req.method, req.path, rec.Code)
		}
	}
}

// The API counts its calls by route pattern and its product errors by code,
// and the code never reaches the client as a header.
func TestCountUsageCountsRoutesAndErrorCodes(t *testing.T) {
	usage := &fakeUsageReport{enabled: true}
	run := usageRun(t, usage, nil)
	run.do(http.MethodGet, "/api/v1/usage-report", "", "")
	run.do(http.MethodGet, "/api/v1/usage-report", "", "")
	rec := run.do(http.MethodPut, "/api/v1/usage-report/enabled", "application/json", `{}`)
	if rec.Header().Get(usageErrorHeader) != "" {
		t.Fatalf("the error code leaked as a header: %v", rec.Header())
	}
	run.do(http.MethodGet, "/api/v1/nodes/no-such-node", "", "")
	run.do(http.MethodGet, "/api/v1/definitely/not/a/route", "", "")
	// The data plane and the login page are not counted as API calls.
	run.do(http.MethodGet, "/v1/models", "", "")

	got := usage.counters.Drain()
	routes := map[string]int{}
	for _, c := range got[usagereport.KindRoute] {
		routes[c.Name] = c.N
	}
	if routes["GET /usage-report"] != 2 || routes["PUT /usage-report/enabled"] != 1 || routes["GET /nodes/{uuid}"] != 1 {
		t.Fatalf("routes = %v", routes)
	}
	for name := range routes {
		if strings.Contains(name, "no-such-node") || strings.Contains(name, "definitely") || strings.HasPrefix(name, "GET /v1") {
			t.Fatalf("counted %q, which names a path rather than a route", name)
		}
	}
	errs := map[string]int{}
	for _, c := range got[usagereport.KindAPIError] {
		errs[c.Name] = c.N
	}
	if errs[ErrorUsageReportInvalid] != 1 {
		t.Fatalf("api errors = %v", errs)
	}
}

func TestCountUsageCountsPanicsAndStillAnswers(t *testing.T) {
	a := &apiHandlers{d: Deps{UsageReport: &fakeUsageReport{}}}
	r := chi.NewRouter()
	r.Use(recoverer)
	r.Use(a.countUsage)
	r.Get("/api/v1/boom", func(http.ResponseWriter, *http.Request) { explode() })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic answered %d", rec.Code)
	}
	panics := a.usageCounters().Drain()[usagereport.KindPanic]
	if len(panics) != 1 || panics[0] != (usagereport.Count{Name: "server/api.explode", N: 1}) {
		t.Fatalf("panics = %v", panics)
	}
}

//go:noinline
func explode() { panic("boom") }

// recoverer stands in for chi's middleware.Recoverer without its stack dump.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Streaming and websocket upgrades go through the counter unchanged.
func TestUsageResponseWriterKeepsFlushAndHijack(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &usageResponseWriter{ResponseWriter: rec, counters: &usagereport.Counters{}}
	w.Flush()
	if !rec.Flushed || rec.Code != http.StatusOK {
		t.Fatalf("flush: flushed %v code %d", rec.Flushed, rec.Code)
	}
	if _, _, err := w.Hijack(); err == nil {
		t.Fatal("a recorder cannot be hijacked; the error must say so")
	}
	if http.NewResponseController(w).Flush() != nil {
		t.Fatal("ResponseController cannot reach the flusher")
	}
}

// Notices are fetched when the channel is: on the sign-in page's prefetch
// and page loads while automatic checks are on, and on a manual check.
func TestNoticesRefreshWithTheUpdateCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		manual bool
		path   string
		method string
		want   int
	}{
		{"prefetch", false, "/api/v1/updates/prefetch", http.MethodPost, 1},
		{"prefetch, checks off", true, "/api/v1/updates/prefetch", http.MethodPost, 0},
		{"page load, checks off", true, "/api/v1/updates", http.MethodGet, 0},
		{"manual check, checks off", true, "/api/v1/updates?refresh=true", http.MethodGet, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notices := &fakeNotices{}
			run, _ := reportingRun(t, &autoCheckServer{manual: tc.manual, id: "0123456789abcdef"})
			run.deps.Notices = notices
			run.router = Router(run.deps)
			run.do(tc.method, tc.path, "", "")
			if notices.refreshes != tc.want {
				t.Fatalf("refreshes = %d, want %d", notices.refreshes, tc.want)
			}
		})
	}
}
