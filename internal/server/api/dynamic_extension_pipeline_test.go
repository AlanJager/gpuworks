package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// With the plugin down, the core still answers first: authentication,
// then the feature gate (hidden 404, locked 403, read-only 403 for a
// write); only an allowed request learns the plugin is down (503, retry).
func TestDynamicRoutesAnswerInOrderWithThePluginDown(t *testing.T) {
	gated := func(r *DynamicRoute) { r.Feature = "acme.backup" }
	rig := newDynamicRig(t, []DynamicRoute{
		dynRoute("GET", "/jobs", DynamicAdmin, gated),
		dynRoute("POST", "/jobs", DynamicAdmin, gated),
		dynRoute("GET", "/secret", DynamicAdmin, func(r *DynamicRoute) { r.Feature = "acme.hidden" }),
		dynRoute("GET", "/open", DynamicAdmin),
	})
	rig.dyn.refuse = func(*DynamicRoute) error {
		return messages.NewProductError(messages.ErrorPluginUnavailable, "the plugin is not running", map[string]any{"plugin": "acme_fake", "state": "crashloop"})
	}
	set := func(a featuregate.Access) {
		rig.src.set(map[featuregate.Key]featuregate.Availability{"acme.backup": {Access: a}, "acme.hidden": {Access: featuregate.Locked}})
	}
	for _, tc := range []struct {
		name, bearer, method, path string
		access                     featuregate.Access
		status                     int
		code                       string
	}{
		{"unauthenticated", "", "GET", "/api/v1/x/acme_fake/jobs", featuregate.Full, 401, "AUTHENTICATION_REQUIRED"},
		{"hidden", rig.session, "GET", "/api/v1/x/acme_fake/secret", featuregate.Full, 404, "FEATURE_UNAVAILABLE"},
		{"locked", rig.session, "GET", "/api/v1/x/acme_fake/jobs", featuregate.Locked, 403, "FEATURE_UNAVAILABLE"},
		{"read-only write", rig.session, "POST", "/api/v1/x/acme_fake/jobs", featuregate.ReadOnly, 403, "FEATURE_READ_ONLY"},
	} {
		set(tc.access)
		if rec := rig.do(tc.bearer, tc.method, tc.path, ""); rec.Code != tc.status || codeOfBody(rec) != tc.code {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	if n := rig.dyn.calls.Load(); n != 0 {
		t.Fatalf("refused requests reached the plugin host %d times", n)
	}
	set(featuregate.ReadOnly)
	rec := rig.do(rig.session, "GET", "/api/v1/x/acme_fake/jobs", "")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" || rig.dyn.last.Load().caller.Access != "read_only" {
		t.Fatalf("read-only read = %d %s, caller %+v", rec.Code, rec.Body, rig.dyn.last.Load().caller)
	}
	rec = rig.do(rig.session, "GET", "/api/v1/x/acme_fake/open", "")
	want := `{"code":"PLUGIN_UNAVAILABLE","error":"the plugin is not running","params":{"plugin":"acme_fake","state":"crashloop"}}`
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" || strings.TrimSpace(rec.Body.String()) != want ||
		rig.dyn.last.Load().caller.Access != "" {
		t.Fatalf("ungated = %d %s", rec.Code, rec.Body)
	}
}

// Each refusal of the plugin host has its status and a literal body.
func TestDynamicRouteRefusalsMapToStatuses(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("POST", "/jobs", DynamicAdmin)})
	for _, tc := range []struct {
		err         error
		status      int
		retry, body string
	}{
		{messages.NewProductError(messages.ErrorPluginBusy, "the plugin is busy", map[string]any{"id": "acme_fake", "limit": 4}), 503, "1",
			`{"code":"PLUGIN_BUSY","error":"the plugin is busy","params":{"id":"acme_fake","limit":4}}`},
		{messages.NewProductError(messages.ErrorPluginTimeout, "the plugin did not answer in time", map[string]any{"id": "acme_fake", "timeout_s": 5, "state": "starting"}), 504, "",
			`{"code":"PLUGIN_TIMEOUT","error":"the plugin did not answer in time","params":{"id":"acme_fake","state":"starting","timeout_s":5}}`},
		{messages.NewProductError(messages.ErrorPluginRouteFailed, "the plugin failed", map[string]any{"id": "acme_fake"}), 502, "",
			`{"code":"PLUGIN_ROUTE_FAILED","error":"the plugin failed","params":{"id":"acme_fake"}}`},
		{messages.NewProductError(messages.ErrorPluginNotInstalled, "gone", map[string]any{"id": "acme_fake"}), 404, "",
			`{"code":"NOT_FOUND","error":"Not Found"}`},
	} {
		rig.dyn.refuse = func(*DynamicRoute) error { return tc.err }
		rec := rig.do(rig.session, "POST", "/api/v1/x/acme_fake/jobs", "")
		if rec.Code != tc.status || rec.Header().Get("Retry-After") != tc.retry || strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Errorf("%v = %d (retry %q) %s", tc.err, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
		}
	}
}

// A body over the route's limit is refused before the plugin sees any of
// it, when the client says its length, and as soon as it outgrows the
// limit when it does not.
func TestDynamicRoutesCapTheBody(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("POST", "/upload", DynamicAdmin, func(r *DynamicRoute) { r.MaxBody = 8 })})
	rec := rig.do(rig.session, "POST", "/api/v1/x/acme_fake/upload", "0123456789")
	want := `{"code":"REQUEST_TOO_LARGE","error":"the request body is larger than this route takes","params":{"limit":8}}`
	if rec.Code != http.StatusRequestEntityTooLarge || strings.TrimSpace(rec.Body.String()) != want || rig.dyn.calls.Load() != 0 {
		t.Fatalf("declared length = %d %s, calls %d", rec.Code, rec.Body, rig.dyn.calls.Load())
	}
	rig.dyn.serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	req := httptestRequest(http.MethodPost, "/api/v1/x/acme_fake/upload", rig.session)
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader("0123456789")), -1
	if rec := serve(rig.h, req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, "POST", "/api/v1/x/acme_fake/upload", "01234567"); rec.Code != http.StatusOK {
		t.Fatalf("at the limit = %d %s", rec.Code, rec.Body)
	}
}

// A plugin's product error passes with its code and params when it is
// one; a malformed one is refused.
func TestDynamicRoutesPassPluginProductErrors(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("GET", "/ok", DynamicAdmin), dynRoute("GET", "/bad", DynamicAdmin), dynRoute("GET", "/success", DynamicAdmin)})
	rig.dyn.serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Gwx-Product-Error", "1")
		w.Header().Set("Retry-After", "7")
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"ACME_BACKUP_RUNNING","message":"a backup is running","params":{"job":"7"},"reason":"conflict"}`)
		case "/bad":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"no code"}`)
		case "/success":
			_, _ = io.WriteString(w, `{"code":"ACME_X","message":"x","reason":"conflict"}`)
		}
	})
	rec := rig.do(rig.session, "GET", "/api/v1/x/acme_fake/ok", "")
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != `{"code":"ACME_BACKUP_RUNNING","error":"a backup is running","params":{"job":"7"}}` || rec.Header().Get("Retry-After") != "7" {
		t.Fatalf("valid product error = %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/bad", "/success"} {
		rec = rig.do(rig.session, "GET", "/api/v1/x/acme_fake"+path, "")
		if rec.Code != http.StatusBadGateway || codeOfBody(rec) != "PLUGIN_ROUTE_RESPONSE_INVALID" {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
}

// A stream outlives its timeout while it writes, is cut off once it goes
// idle, and a stream or a plain response that fails after its headers
// aborts the connection rather than end as a short success.
func TestDynamicStreamsAndAbortedResponses(t *testing.T) {
	stream := func(r *DynamicRoute) { r.Stream, r.Timeout = true, 50*time.Millisecond }
	dyn := newFakeDynamic(t, dynRoute("GET", "/feed", DynamicPublic, stream), dynRoute("GET", "/stall", DynamicPublic, stream),
		dynRoute("GET", "/short", DynamicPublic))
	dyn.idle = 150 * time.Millisecond
	srv := httptest.NewServer(Router(Deps{Extensions: ExtensionDeps{Dynamic: dyn.ext()}}))
	t.Cleanup(srv.Close)
	dyn.serve(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed":
			for range 5 {
				_, _ = io.WriteString(w, "tick\n")
				w.(http.Flusher).Flush()
				time.Sleep(60 * time.Millisecond)
			}
		case "/stall":
			_, _ = io.WriteString(w, "tick\n")
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Second)
		case "/short":
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "only part")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	})
	get := func(path string) (int, string, error) {
		resp, err := http.Get(srv.URL + "/api/v1/x/acme_fake" + path)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}
	if status, body, err := get("/feed"); status != 200 || body != strings.Repeat("tick\n", 5) || err != nil {
		t.Fatalf("a writing stream = %d %q %v", status, body, err)
	}
	start := time.Now()
	if _, body, err := get("/stall"); err == nil || body != "tick\n" || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("an idle stream = %q %v after %v", body, err, time.Since(start))
	}
	if _, _, err := get("/short"); err == nil {
		t.Fatal("a response that failed after its headers ended cleanly")
	}
}

// The assistant reads only plugin routes marked ai_readable, and the
// route list it searches has them.
func TestDynamicRoutesAndTheAssistant(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("GET", "/jobs", DynamicAdmin, func(r *DynamicRoute) { r.AIReadable = true }), dynRoute("GET", "/keys", DynamicAdmin)})
	read := func(path string) *httptest.ResponseRecorder {
		req := httptestRequest(http.MethodGet, path, rig.session)
		return serve(rig.h, req.WithContext(context.WithValue(req.Context(), assistantReadKey{}, true)))
	}
	if rec := read("/api/v1/x/acme_fake/keys"); rec.Code != http.StatusNotFound || rig.dyn.calls.Load() != 0 {
		t.Fatalf("assistant on an unreadable route = %d %s", rec.Code, rec.Body)
	}
	if rec := read("/api/v1/x/acme_fake/jobs"); rec.Code != http.StatusOK {
		t.Fatalf("assistant on a readable route = %d %s", rec.Code, rec.Body)
	}
	a := &apiHandlers{d: Deps{Extensions: ExtensionDeps{Dynamic: rig.dyn.ext()}}}
	a.dynamic = &dynamicEndpoint{ext: rig.dyn.ext()}
	if routes := a.getRoutes(); !slices.Contains(routes, "/api/v1/x/acme_fake/jobs") || slices.Contains(routes, "/api/v1/x/acme_fake/keys") || slices.Contains(routes, "/api/v1/x/*") {
		t.Fatalf("assistant routes = %v", routes)
	}
}

// A compiled extension's namespace keeps priority: a wrong method answers
// the core 405 and an unknown path the core 404, never a plugin.
func TestCompiledNamespacesKeepPriority(t *testing.T) {
	ext := fakeExtension{name: "acme_fake", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/acme_fake/items", Community: true}}}
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("POST", "/items", DynamicAdmin), dynRoute("GET", "/other", DynamicAdmin)}, func(d *Deps) {
		d.Extensions.Extensions = []Extension{ext}
	})
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/x/acme_fake/items", 200}, {"POST", "/api/v1/x/acme_fake/items", 405}, {"GET", "/api/v1/x/acme_fake/other", 404}} {
		if rec := rig.do(rig.session, tc.method, tc.path, ""); rec.Code != tc.status {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
	if rig.dyn.calls.Load() != 0 {
		t.Fatal("a request in a compiled namespace reached a plugin")
	}
	// A miss and a wrong method in a plugin's own namespace are the core's
	// answers too.
	for path, status := range map[string]int{"/api/v1/x/acme_other/none": 404} {
		if rec := rig.do(rig.session, "GET", path, ""); rec.Code != status || strings.TrimSpace(rec.Body.String()) != `{"code":"NOT_FOUND","error":"Not Found"}` {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
}

// With nothing installed the mount adds nothing to the inventory; each
// installed route adds exactly its line, beside a compiled extension's.
func TestDynamicRoutesInTheInventory(t *testing.T) {
	empty, err := RouteInventory(Router(Deps{Extensions: ExtensionDeps{Dynamic: newFakeDynamic(t).ext()}}))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Split(strings.TrimSpace(fullRouteInventory), "\n"); !slices.Equal(empty, want) {
		t.Fatalf("the mount changed the inventory:\n%s", strings.Join(empty, "\n"))
	}
	ext := fakeExtension{name: "ops", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/ops/audit", Community: true}}}
	dyn := newFakeDynamic(t, dynRoute("GET", "/", DynamicPublic), dynRoute("POST", "/jobs/{id}", DynamicAdminSession, func(r *DynamicRoute) {
		r.Feature, r.Action = "acme.backup", "acme_fake.jobs_run"
	}))
	got, err := RouteInventory(Router(Deps{Extensions: ExtensionDeps{Extensions: []Extension{ext}, Dynamic: dyn.ext()}}))
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, line := range got {
		if !slices.Contains(empty, line) {
			added = append(added, line)
		}
	}
	want := []string{
		"GET /api/v1/x/acme_fake/ [access=public] [action=acme_fake.x] [ungated] [x=acme_fake@1.0.0]",
		"GET /api/v1/x/ops/audit [access=read] [community]",
		"POST /api/v1/x/acme_fake/jobs/{id} [access=admin_session] [action=acme_fake.jobs_run] [feature=acme.backup] [x=acme_fake@1.0.0]",
	}
	if !slices.Equal(added, want) {
		t.Fatalf("inventory added:\n%s", strings.Join(added, "\n"))
	}
}

// A write to a plugin route leaves an audit line naming the caller, the
// route and the outcome; a read leaves none.
func TestDynamicRouteWritesAreAudited(t *testing.T) {
	var logs bytes.Buffer
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("POST", "/jobs", DynamicAdmin, func(r *DynamicRoute) { r.Action = "acme_fake.jobs_create" }),
		dynRoute("GET", "/jobs", DynamicAdmin)}, func(d *Deps) { d.Log = slog.New(slog.NewJSONHandler(&logs, nil)) })
	rig.do(rig.session, "GET", "/api/v1/x/acme_fake/jobs", "")
	rig.do(rig.session, "POST", "/api/v1/x/acme_fake/jobs", "")
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "plugin route" {
			lines = append(lines, m)
		}
	}
	if len(lines) != 1 || lines[0]["audit"] != true || lines[0]["action"] != "acme_fake.jobs_create" || lines[0]["method"] != "POST" ||
		lines[0]["user"] != "admin" || lines[0]["caller"] != "session" || lines[0]["status"] != float64(200) || lines[0]["plugin"] != "acme_fake" {
		t.Fatalf("audit lines = %v", lines)
	}
}
