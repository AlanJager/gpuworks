package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func testconnStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

const testconnStoredKey = "stored-secret-key"

// savedTestconnProvider stores one provider of the test kind and returns the
// service, its driver (with the validation of the save forgotten) and the id.
func savedTestconnProvider(t *testing.T, st *store.Store) (*provider.Service, *testconnDriver, uint) {
	t.Helper()
	box, err := auth.NewSecretbox("api-provider-testconn")
	if err != nil {
		t.Fatal(err)
	}
	drv := &testconnDriver{}
	svc := provider.NewService(st, box)
	svc.SetDriver(drv)
	svc.SetConnectionTestRunner(hermeticTestconnRunner(nil, nil))
	p, err := svc.Add(context.Background(), drv.Name(), "saved", provider.Credentials{
		Endpoint: "http://cloud.example:8080", APIKey: testconnStoredKey, Extra: map[string]string{"zone": "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	drv.validated = nil
	return svc, drv, p.ID
}

func savedTestconnRequest(id uint, body string) *http.Request {
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/"+strconv.FormatUint(uint64(id), 10)+"/test", reader)
	return req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": strconv.FormatUint(uint64(id), 10)}))
}

// A stored provider is tested with its stored credentials when the request
// has no body, and never returns them.
func TestSavedProviderTestStreamsStoredCredentials(t *testing.T) {
	st := testconnStore(t)
	svc, drv, id := savedTestconnProvider(t, st)
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	stored := provider.Credentials{Endpoint: "http://cloud.example:8080", APIKey: testconnStoredKey, Extra: map[string]string{"zone": "a"}}
	for name, body := range map[string]string{"no body": "", "empty object": `{}`, "blank fields": `{"endpoint":" ","api_key":"","extra":{"zone":""}}`} {
		t.Run(name, func(t *testing.T) {
			drv.validated = nil
			rec := newNotedRecorder()
			h.testSavedProviderConnection(rec, savedTestconnRequest(id, body))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status = %d, content-type = %q, body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
			}
			wantSSE(t, rec.Body.String(), testconnReachable...)
			if !reflect.DeepEqual(drv.validated, []provider.Credentials{stored}) {
				t.Fatalf("validated %+v, want the stored credentials", drv.validated)
			}
			if strings.Contains(rec.Body.String(), testconnStoredKey) {
				t.Fatalf("stored secret returned: %s", rec.Body)
			}
		})
	}
}

// A body overrides the stored fields it fills in, for this test only; a field
// it leaves out keeps the stored value, secrets included.
func TestSavedProviderTestMergesOverrides(t *testing.T) {
	st := testconnStore(t)
	svc, drv, id := savedTestconnProvider(t, st)
	var dialed []string
	svc.SetConnectionTestRunner(hermeticTestconnRunner(nil, &dialed))
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}

	rec := httptest.NewRecorder()
	h.testSavedProviderConnection(rec, savedTestconnRequest(id, `{"endpoint":"http://other.example:9090"}`))
	frames := append([][2]string{}, testconnReachable...)
	frames[0] = [2]string{"check", `{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"other.example:9090","scheme":"http"}}`}
	frames[3] = [2]string{"check", `{"seq":4,"name":"tcp_connect","status":"ok","duration_ms":0,"detail":{"address":"other.example:9090","rtt_ms":0}}`}
	wantSSE(t, rec.Body.String(), frames...)
	want := provider.Credentials{Endpoint: "http://other.example:9090", APIKey: testconnStoredKey, Extra: map[string]string{"zone": "a"}}
	if !reflect.DeepEqual(drv.validated, []provider.Credentials{want}) {
		t.Fatalf("validated %+v, want the stored secret with the overriding endpoint", drv.validated)
	}
	if !reflect.DeepEqual(dialed, []string{"other.example:9090"}) {
		t.Fatalf("dialed %v", dialed)
	}
	if strings.Contains(rec.Body.String(), testconnStoredKey) {
		t.Fatalf("stored secret returned: %s", rec.Body)
	}

	drv.validated = nil
	rec = httptest.NewRecorder()
	h.testSavedProviderConnection(rec, savedTestconnRequest(id, `{"api_key":"typed-key","extra":{"pool":"p1"}}`))
	want = provider.Credentials{Endpoint: "http://cloud.example:8080", APIKey: "typed-key", Extra: map[string]string{"zone": "a", "pool": "p1"}}
	if !reflect.DeepEqual(drv.validated, []provider.Credentials{want}) {
		t.Fatalf("validated %+v, want %+v", drv.validated, want)
	}

	// Nothing was saved: the next plain test uses the stored credentials.
	drv.validated = nil
	h.testSavedProviderConnection(httptest.NewRecorder(), savedTestconnRequest(id, ""))
	want = provider.Credentials{Endpoint: "http://cloud.example:8080", APIKey: testconnStoredKey, Extra: map[string]string{"zone": "a"}}
	if !reflect.DeepEqual(drv.validated, []provider.Credentials{want}) {
		t.Fatalf("after overrides: validated %+v", drv.validated)
	}
}

// A run that cannot start (the stored credentials do not decrypt, or the
// kind's driver is gone) ends the stream with a coded done frame; the cause
// goes to the server log, not the wire.
func TestSavedProviderTestRunErrorIsACodedDoneFrame(t *testing.T) {
	st := testconnStore(t)
	_, drv, id := savedTestconnProvider(t, st)
	otherBox, err := auth.NewSecretbox("a-different-secret")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() *provider.Service{
		"credentials do not decrypt": func() *provider.Service {
			broken := provider.NewService(st, otherBox)
			broken.SetDriver(drv)
			return broken
		},
		"driver not registered": func() *provider.Service {
			// The provider row stays; this service has no driver for its kind.
			return provider.NewService(st, mustTestconnBox(t))
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureDefaultLog(t)
			service := build()
			service.SetConnectionTestRunner(hermeticTestconnRunner(nil, nil))
			drv.validated = nil
			rec := newNotedRecorder()
			(&apiHandlers{d: Deps{Store: st, Providers: service}}).testSavedProviderConnection(rec, savedTestconnRequest(id, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			wantSSE(t, rec.Body.String(), [2]string{"done", `{"ok":false,"code":"PROVIDER_TEST_UNAVAILABLE"}`})
			if !reflect.DeepEqual(rec.noted, []string{messages.ErrorProviderTestUnavailable}) {
				t.Fatalf("access log noted %v", rec.noted)
			}
			if !strings.Contains(logs.String(), "provider connection test did not finish") {
				t.Fatalf("the cause was not logged: %s", logs)
			}
			if len(drv.validated) != 0 {
				t.Fatal("credentials were validated on a run that could not start")
			}
		})
	}
}

// captureDefaultLog collects what the default logger writes during the test.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func mustTestconnBox(t *testing.T) *auth.Secretbox {
	t.Helper()
	box, err := auth.NewSecretbox("api-provider-testconn")
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestSavedProviderTestUnknownProviderIs404(t *testing.T) {
	st, err := store.Open("file:api_provider_testconn_404?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: provider.NewService(st, nil)}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/999/test", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": "999"}))
	rec := httptest.NewRecorder()
	h.testSavedProviderConnection(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
}
