package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	_ "gpuworks.ai/gpuworks/internal/server/provider/all"
	"gpuworks.ai/gpuworks/internal/server/provider/testconn"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// parseSSEFrames splits a recorded SSE body into (event, dataJSON) pairs.
func parseSSEFrames(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, frame := range strings.Split(body, "\n\n") {
		if frame == "" {
			continue
		}
		var event, data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data += strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, [2]string{event, data})
	}
	return out
}

func TestProviderTestUnknownKindIs400(t *testing.T) {
	h := apiHandlers{d: Deps{Providers: provider.NewService(nil, nil)}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/test",
		strings.NewReader(`{"kind":"nope","endpoint":"http://192.0.2.1"}`))
	rec := httptest.NewRecorder()
	h.testProviderConnection(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != messages.ErrorProviderKindUnknown {
		t.Fatalf("code = %q, want %q", body.Code, messages.ErrorProviderKindUnknown)
	}
}

// testconnDriver is a provider kind whose credential validation records what
// it was asked and can be made to fail.
type testconnDriver struct {
	apiProviderTestDriver
	validateErr error
	validated   []provider.Credentials
}

func (d *testconnDriver) Name() string { return "api-testconn-provider" }

func (d *testconnDriver) ValidateCredentials(_ context.Context, creds provider.Credentials) error {
	d.validated = append(d.validated, creds)
	return d.validateErr
}

type testconnRoundTrip func(*http.Request) (*http.Response, error)

func (f testconnRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var testconnNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// hermeticTestconnRunner reaches nothing real: the resolver answers a
// documentation address, a dial returns dialErr or one end of an in-memory
// pipe, and the clock-skew request is answered in process.
func hermeticTestconnRunner(dialErr error, dialed *[]string) *testconn.Runner {
	return &testconn.Runner{
		Now:        func() time.Time { return testconnNow },
		LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil },
		Dial: func(_ context.Context, _, address string) (net.Conn, error) {
			if dialed != nil {
				*dialed = append(*dialed, address)
			}
			if dialErr != nil {
				return nil, dialErr
			}
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
		Proxy:  func(*http.Request) (*url.URL, error) { return nil, nil },
		Getenv: func(string) string { return "" },
		HTTPClient: &http.Client{Transport: testconnRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Date": []string{testconnNow.Format(http.TimeFormat)}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
	}
}

// sseBody is the literal stream of the given frames.
func sseBody(frames ...[2]string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("event: " + f[0] + "\ndata: " + f[1] + "\n\n")
	}
	return b.String()
}

func wantSSE(t *testing.T, got string, frames ...[2]string) {
	t.Helper()
	if want := sseBody(frames...); got != want {
		t.Fatalf("stream\n got %s\nwant %s", got, want)
	}
}

// The frames of a reachable plain-HTTP endpoint whose credentials the driver
// accepts.
var testconnReachable = [][2]string{
	{"check", `{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"cloud.example:8080","scheme":"http"}}`},
	{"check", `{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":1}}`},
	{"check", `{"seq":3,"name":"dns_resolve","status":"ok","duration_ms":0,"detail":{"ips":["192.0.2.10"]}}`},
	{"check", `{"seq":4,"name":"tcp_connect","status":"ok","duration_ms":0,"detail":{"address":"cloud.example:8080","rtt_ms":0}}`},
	{"check", `{"seq":5,"name":"tls_handshake","status":"ok","duration_ms":0,"detail":{"mode":"plain_http"}}`},
	{"check", `{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"none"}}`},
	{"check", `{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":0}}`},
	{"check", `{"seq":8,"name":"auth_request","status":"ok","duration_ms":0}`},
	{"done", `{"ok":true}`},
}

// notedRecorder is a response writer that records the product error the
// access log would write on the request's line.
type notedRecorder struct {
	*httptest.ResponseRecorder
	noted []string
}

func (w *notedRecorder) NoteProductError(code string, _ map[string]any) {
	w.noted = append(w.noted, code)
}

func newNotedRecorder() *notedRecorder {
	return &notedRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func TestProviderTestStreamsLiteralFrames(t *testing.T) {
	const body = `{"kind":"api-testconn-provider","endpoint":"http://cloud.example:8080","api_key":"typed-key","extra":{"zone":"a"}}`
	t.Run("all checks pass", func(t *testing.T) {
		drv := &testconnDriver{}
		svc := provider.NewService(nil, nil)
		svc.SetDriver(drv)
		svc.SetConnectionTestRunner(hermeticTestconnRunner(nil, nil))
		rec := newNotedRecorder()
		(&apiHandlers{d: Deps{Providers: svc}}).testProviderConnection(rec,
			httptest.NewRequest(http.MethodPost, "/api/v1/providers/test", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
		}
		if ct, cc := rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"); ct != "text/event-stream" || cc != "no-cache" {
			t.Fatalf("content-type = %q, cache-control = %q", ct, cc)
		}
		wantSSE(t, rec.Body.String(), testconnReachable...)
		want := provider.Credentials{Endpoint: "http://cloud.example:8080", APIKey: "typed-key", Extra: map[string]string{"zone": "a"}}
		if !reflect.DeepEqual(drv.validated, []provider.Credentials{want}) {
			t.Fatalf("validated %+v, want the request's credentials", drv.validated)
		}
		if len(rec.noted) != 0 {
			t.Fatalf("a passing test noted %v on the access log", rec.noted)
		}
	})
	t.Run("a check fails", func(t *testing.T) {
		drv := &testconnDriver{validateErr: errors.New("status 401: vendor echoes typed-key")}
		svc := provider.NewService(nil, nil)
		svc.SetDriver(drv)
		svc.SetConnectionTestRunner(hermeticTestconnRunner(nil, nil))
		rec := newNotedRecorder()
		(&apiHandlers{d: Deps{Providers: svc}}).testProviderConnection(rec,
			httptest.NewRequest(http.MethodPost, "/api/v1/providers/test", strings.NewReader(body)))
		// The outcome is payload: a failed test still answers 200.
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		frames := append(append([][2]string{}, testconnReachable[:7]...),
			[2]string{"check", `{"seq":8,"name":"auth_request","status":"failed","duration_ms":0,"code":"PROVIDER_AUTH_FAILED","hint":"auth"}`},
			[2]string{"done", `{"ok":false,"failed":["auth_request"]}`})
		wantSSE(t, rec.Body.String(), frames...)
		if !reflect.DeepEqual(rec.noted, []string{messages.ErrorProviderAuthFailed}) {
			t.Fatalf("access log noted %v, want the first failed check's code", rec.noted)
		}
	})
	t.Run("failed lists names in report order", func(t *testing.T) {
		svc := provider.NewService(nil, nil)
		svc.SetDriver(&testconnDriver{})
		svc.SetConnectionTestRunner(hermeticTestconnRunner(syscall.ECONNREFUSED, nil))
		rec := newNotedRecorder()
		(&apiHandlers{d: Deps{Providers: svc}}).testProviderConnection(rec, httptest.NewRequest(http.MethodPost, "/api/v1/providers/test",
			strings.NewReader(`{"kind":"api-testconn-provider","endpoint":"http://cloud.example:8080"}`)))
		const skipped = `"status":"skipped","duration_ms":0,"detail":{"depends":"tcp_connect"},"hint":"depends_failed"}`
		wantSSE(t, rec.Body.String(),
			testconnReachable[0],
			[2]string{"check", `{"seq":2,"name":"credentials_shape","status":"failed","duration_ms":0,"code":"PROVIDER_CREDENTIAL_MISSING","detail":{"field":"api_key"},"hint":"credential_missing"}`},
			testconnReachable[2],
			[2]string{"check", `{"seq":4,"name":"tcp_connect","status":"failed","duration_ms":0,"code":"PROVIDER_TCP_REFUSED","hint":"tcp_refused"}`},
			[2]string{"check", `{"seq":5,"name":"tls_handshake",` + skipped},
			testconnReachable[5],
			[2]string{"check", `{"seq":7,"name":"clock_skew",` + skipped},
			[2]string{"check", `{"seq":8,"name":"auth_request","status":"skipped","duration_ms":0,"detail":{"depends":"credentials_shape"},"hint":"depends_failed"}`},
			[2]string{"done", `{"ok":false,"failed":["credentials_shape","tcp_connect"]}`})
		if !reflect.DeepEqual(rec.noted, []string{messages.ErrorProviderCredentialMissing}) {
			t.Fatalf("access log noted %v", rec.noted)
		}
	})
}

// zstack declares its own credential schema (endpoint + access key pair) and
// ships vendor checks; credentials_shape passes with those fields present,
// and its vendor checks are skipped behind a refused connection.
func TestProviderTestZStackSchemaAndVendorChecks(t *testing.T) {
	svc := provider.NewService(nil, nil)
	var dialed []string
	svc.SetConnectionTestRunner(hermeticTestconnRunner(syscall.ECONNREFUSED, &dialed))
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Providers: svc}}).testProviderConnection(rec, httptest.NewRequest(http.MethodPost, "/api/v1/providers/test",
		strings.NewReader(`{"kind":"zstack","endpoint":"http://cloud.example:8080/zstack","public_key":"ak","private_key":"sk"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var names []string
	statuses := map[string]string{}
	var done string
	for _, f := range parseSSEFrames(t, rec.Body.String()) {
		if f[0] == "done" {
			done = f[1]
			continue
		}
		var c struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(f[1]), &c); err != nil {
			t.Fatal(err)
		}
		names = append(names, c.Name)
		statuses[c.Name] = c.Status
	}
	want := []string{"endpoint_format", "credentials_shape", "dns_resolve", "tcp_connect", "tls_handshake", "proxy_detect", "clock_skew",
		"auth_request", "protocol_shape", "api_endpoints", "read_permissions", "resource_summary"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("checks = %v, want %v", names, want)
	}
	if statuses["credentials_shape"] != "ok" || statuses["tcp_connect"] != "failed" {
		t.Fatalf("statuses = %v", statuses)
	}
	for _, name := range want[7:] {
		if statuses[name] != "skipped" {
			t.Fatalf("%s = %s, want skipped behind tcp_connect", name, statuses[name])
		}
	}
	if done != `{"ok":false,"failed":["tcp_connect"]}` {
		t.Fatalf("done = %s", done)
	}
	if !reflect.DeepEqual(dialed, []string{"cloud.example:8080"}) {
		t.Fatalf("dialed %v", dialed)
	}
}

func TestProviderTestMalformedJSONIs400(t *testing.T) {
	st := testconnStore(t)
	svc, _, id := savedTestconnProvider(t, st)
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	rec := httptest.NewRecorder()
	h.testProviderConnection(rec, httptest.NewRequest(http.MethodPost, "/api/v1/providers/test", strings.NewReader(`{"kind":`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("explicit test status = %d, body %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.testSavedProviderConnection(rec, savedTestconnRequest(id, `{"endpoint":`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stored test status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
		t.Fatal("a rejected request started a stream")
	}
}

func TestProviderTestWithoutProvidersIs503(t *testing.T) {
	h := apiHandlers{d: Deps{}}
	rec := httptest.NewRecorder()
	h.testProviderConnection(rec, httptest.NewRequest(http.MethodPost, "/api/v1/providers/test", strings.NewReader(`{"kind":"zstack"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("explicit test status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.testSavedProviderConnection(rec, savedTestconnRequest(1, ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stored test status = %d", rec.Code)
	}
}

// plainWriter is a response writer that cannot flush.
type plainWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *plainWriter) Header() http.Header         { return w.header }
func (w *plainWriter) WriteHeader(status int)      { w.status = status }
func (w *plainWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func TestProviderTestNeedsAFlushingWriter(t *testing.T) {
	drv := &testconnDriver{}
	svc := provider.NewService(nil, nil)
	svc.SetDriver(drv)
	svc.SetConnectionTestRunner(hermeticTestconnRunner(nil, nil))
	w := &plainWriter{header: http.Header{}}
	(&apiHandlers{d: Deps{Providers: svc}}).testProviderConnection(w, httptest.NewRequest(http.MethodPost, "/api/v1/providers/test",
		strings.NewReader(`{"kind":"api-testconn-provider","endpoint":"http://cloud.example:8080","api_key":"k"}`)))
	if w.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s", w.status, w.body.String())
	}
	if strings.Contains(w.body.String(), "event:") || len(drv.validated) != 0 {
		t.Fatalf("a run started on a writer that cannot stream: %s", w.body.String())
	}
}

// A client that closes the dialog cancels the request: the run stops, no done
// frame follows and the access log notes no product error.
func TestProviderTestCancelledRequestEndsSilently(t *testing.T) {
	logs := captureDefaultLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	drv := &testconnDriver{}
	svc := provider.NewService(nil, nil)
	svc.SetDriver(drv)
	runner := hermeticTestconnRunner(nil, nil)
	runner.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		cancel()
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	}
	svc.SetConnectionTestRunner(runner)
	rec := newNotedRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/test",
		strings.NewReader(`{"kind":"api-testconn-provider","endpoint":"http://cloud.example:8080","api_key":"k"}`)).WithContext(ctx)
	(&apiHandlers{d: Deps{Providers: svc}}).testProviderConnection(rec, req)
	wantSSE(t, rec.Body.String(), testconnReachable[:3]...)
	if len(rec.noted) != 0 {
		t.Fatalf("a cancelled test noted %v on the access log", rec.noted)
	}
	if len(drv.validated) != 0 || logs.Len() != 0 {
		t.Fatalf("cancelled run validated %d times, logged %q", len(drv.validated), logs)
	}
}

func TestProviderTestRoutesRequireAuthentication(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-provider-test-auth-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := testconnStore(t)
	svc, drv, id := savedTestconnProvider(t, st)
	r := Router(Deps{AuthSigner: signer, Store: st, Providers: svc})
	paths := map[string]string{
		"/api/v1/providers/test": `{"kind":"api-testconn-provider","endpoint":"http://cloud.example:8080","api_key":"k"}`,
		"/api/v1/providers/" + strconv.FormatUint(uint64(id), 10) + "/test": ``,
	}
	for path, body := range paths {
		for name, header := range map[string]string{"no token": "", "bad token": "Bearer not-a-token"} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s with %s: status = %d, body %s", path, name, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "event:") {
				t.Fatalf("%s with %s streamed checks: %s", path, name, rec.Body)
			}
		}
		// The same request with a session runs the test through the router.
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s authenticated: status = %d, body %s", path, rec.Code, rec.Body)
		}
		frames := parseSSEFrames(t, rec.Body.String())
		if last := frames[len(frames)-1]; last != [2]string{"done", `{"ok":true}`} {
			t.Fatalf("%s authenticated: last frame = %v", path, last)
		}
	}
	if len(drv.validated) != 2 {
		t.Fatalf("validated %d times, want once per authenticated request", len(drv.validated))
	}
}

// POST /providers/test is a static segment that must win over
// POST /providers/{id}/test: an unknown kind answers 400 from
// testProviderConnection, while the {id} route would 404 on GetProvider.
func TestRouterProviderTestStaticRouteNotSwallowed(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-provider-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:api_provider_testconn_router?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{AuthSigner: signer, Store: st, Providers: provider.NewService(st, nil)})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/test",
		strings.NewReader(`{"kind":"nope","endpoint":"http://192.0.2.1"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("static /providers/test status = %d, body %s", rec.Code, rec.Body)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/providers/999/test", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("/providers/{id}/test status = %d, body %s", rec2.Code, rec2.Body)
	}
}
