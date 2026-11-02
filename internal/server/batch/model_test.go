package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModels answers a model lookup from a table; a name it does not list is
// unknown, one in failing is a store failure.
type fakeModels struct {
	states  map[string]ModelState
	failing map[string]bool
	listErr error
}

// List is every name of the table but the unknown ones, the best first and
// then by name, as Models.List says.
func (f fakeModels) List() ([]ModelEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []ModelEntry{}
	for name, state := range f.states {
		if state != ModelUnknown {
			out = append(out, ModelEntry{Name: name, State: state})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State.Better(out[j].State)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (f fakeModels) Lookup(name string) (ModelState, error) {
	if f.failing[name] {
		return ModelUnknown, errors.New("store is down")
	}
	return f.states[name], nil
}

var platformModels = fakeModels{states: map[string]ModelState{"real": ModelAvailable, "held": ModelSleepHeld, "m": ModelAvailable}}

// multipartForm is a console create form: the fields, then the file.
func multipartForm(t *testing.T, fields map[string]string, file string) *multipart.Reader {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if v != "" {
			_ = mw.WriteField(k, v)
		}
	}
	w, _ := mw.CreateFormFile("file", "in.jsonl")
	_, _ = w.Write([]byte(file))
	_ = mw.Close()
	return multipart.NewReader(&buf, mw.Boundary())
}

func modelOf(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Model
}

// The override replaces body.model of every line, on every attempt, and
// touches nothing else of the body.
func TestOverrideRewritesTheModelOfEveryLineAndEveryRetry(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	r := newRig(t, Config{Models: platformModels}, func(_ context.Context, _ Owner, _ string, body []byte) Reply {
		mu.Lock()
		defer mu.Unlock()
		var line struct {
			Messages []struct{ Content string } `json:"messages"`
			MaxTok   int                        `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &line); err != nil {
			t.Errorf("rewritten body is not JSON: %s", body)
		}
		id := line.Messages[0].Content
		seen[id] = append(seen[id], modelOf(body)+fmt.Sprint(line.MaxTok))
		if len(seen[id]) == 1 && id == "q1" {
			return Reply{Status: 503, Body: []byte(`asleep`)}
		}
		return okReply(`{}`)
	})
	r.start()
	content := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"other-1","max_tokens":7,"messages":[{"role":"user","content":"q0"}]}}
{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"messages":[{"role":"user","content":"q1"}],"model":"other-2","max_tokens":7}}
{"custom_id":"c","method":"POST","url":"/v1/chat/completions","body":{"model":"real","max_tokens":7,"messages":[{"role":"user","content":"q2"}]}}
`
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h", Model: "real"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the retry's backoff", func() bool { return retryDelays(r.clock) > 0 })
	r.clock.Advance(time.Minute)
	done := r.await(b.ID, StatusCompleted)
	if done.Model != "real" || done.Completed != 3 {
		t.Fatalf("batch = %+v", done)
	}
	for id, attempts := range seen {
		want := 1
		if id == "q1" {
			want = 2
		}
		if len(attempts) != want {
			t.Fatalf("%s sent %d times, want %d", id, len(attempts), want)
		}
		for _, a := range attempts {
			if a != "real7" {
				t.Errorf("%s sent as %q, want model real and max_tokens 7", id, a)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("lines sent: %v", seen)
	}
}

// Without an override every line goes with its own body, byte for byte.
func TestNoOverrideSendsEachLineUntouched(t *testing.T) {
	var mu sync.Mutex
	var got []string
	r := newRig(t, Config{Models: fakeModels{states: map[string]ModelState{"whatever": ModelAvailable}}}, func(_ context.Context, _ Owner, _ string, body []byte) Reply {
		mu.Lock()
		got = append(got, string(body))
		mu.Unlock()
		return okReply(`{}`)
	})
	r.start()
	b := r.batchFrom(`{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"whatever",  "messages":[]}}` + "\n")
	done := r.await(b.ID, StatusCompleted)
	if done.Model != "" || len(got) != 1 || got[0] != `{"model":"whatever",  "messages":[]}` {
		t.Fatalf("batch model %q sent %q", done.Model, got)
	}
}

func TestWithModel(t *testing.T) {
	for _, tc := range []struct{ name, body, model, want string }{
		{"replaces", `{"model":"a","x":1}`, "b", `{"model":"b","x":1}`},
		{"adds one the line lacks", `{"x":1}`, "b", `{"model":"b","x":1}`},
		{"escapes the name", `{"model":"a"}`, `we"ird`, `{"model":"we\"ird"}`},
		{"keeps nested objects", `{"model":"a","m":{"model":"inner"}}`, "b", `{"model":"b","m":{"model":"inner"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got, want any
			raw := withModel([]byte(tc.body), tc.model)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("not JSON: %s", raw)
			}
			_ = json.Unmarshal([]byte(tc.want), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s, want %s", raw, tc.want)
			}
		})
	}
	// Without an override, and for a body that is not an object, the bytes go as they are.
	for _, tc := range []struct{ body, model string }{{`{"model":"a",  "x":1}`, ""}, {`[1]`, "b"}, {`null`, "b"}, {`nope`, "b"}} {
		if got := string(withModel([]byte(tc.body), tc.model)); got != tc.body {
			t.Errorf("withModel(%s, %q) = %s", tc.body, tc.model, got)
		}
	}
}

func TestCreateBatchChecksTheModel(t *testing.T) {
	r := newRig(t, Config{Models: fakeModels{
		states:  map[string]ModelState{"real": ModelAvailable, "held": ModelSleepHeld, "m": ModelAvailable},
		failing: map[string]bool{"broken": true},
	}}, echo)
	r.start()
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(1)))
	if err != nil {
		t.Fatal(err)
	}
	create := func(model string) (Batch, error) {
		return r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h", Model: model})
	}

	for _, name := range []string{"ghost", "stopped-one", strings.Repeat("x", 300)} {
		_, err := create(name)
		var e *Error
		if !errors.As(err, &e) || e.Status != 400 || e.Code != "model_not_found" || e.Param != "model" || e.Model != name {
			t.Fatalf("%.10s: err = %#v", name, err)
		}
	}
	if len(r.repo.batches) != 0 {
		t.Fatalf("a refused model left %d batches", len(r.repo.batches))
	}
	if _, err := create("broken"); err == nil || errors.As(err, new(*Error)) {
		t.Fatalf("a lookup failure is a server error, not a refusal: %v", err)
	}
	if len(r.repo.batches) != 0 {
		t.Fatalf("a failed lookup left %d batches", len(r.repo.batches))
	}

	b, err := create("real")
	if err != nil || b.Model != "real" || len(b.Warnings) != 0 {
		t.Fatalf("available: %+v err=%v", b, err)
	}
	stored, _ := r.repo.GetBatch(b.ID)
	if stored.Model != "real" || len(stored.Warnings) != 0 {
		t.Fatalf("stored = %+v", stored)
	}

	b, err = create("held")
	if err != nil || b.Model != "held" || len(b.Warnings) != 1 || b.Warnings[0].Code != WarningModelSleepHeld {
		t.Fatalf("held: %+v err=%v", b, err)
	}
	if stored, _ := r.repo.GetBatch(b.ID); len(stored.Warnings) != 0 {
		t.Fatalf("a warning was stored: %+v", stored.Warnings)
	}

	b, err = create("")
	if err != nil || b.Model != "" || len(b.Warnings) != 0 {
		t.Fatalf("no model: %+v err=%v", b, err)
	}
}

func TestWithoutAModelCheckerAnyNameIsAccepted(t *testing.T) {
	r := newRig(t, Config{}, echo)
	r.start()
	f, _ := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(1)))
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h", Model: "anything"})
	if err != nil || b.Model != "anything" {
		t.Fatalf("batch = %+v err=%v", b, err)
	}
}

// The /v1 extension field: accepted, echoed back only when set, with the
// warning on the create response alone.
func TestV1BatchModelExtension(t *testing.T) {
	v := &v1Rig{}
	r := newRig(t, Config{Models: platformModels}, echo)
	r.start()
	v.rig = r
	v.h = r.svc.V1Handler(func(*http.Request) (Owner, bool) { return testOwner, true })
	file := v.uploadBatchFile("k", inputLines(1))["id"].(string)
	create := func(extra string) (int, map[string]any) {
		body := fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"%s}`, file, extra)
		rec := v.do("k", "POST", "/v1/batches", "application/json", strings.NewReader(body))
		return rec.Code, decode(t, rec.Body.Bytes())
	}

	code, obj := create("")
	if _, has := obj["model"]; code != 200 || has {
		t.Fatalf("no model: %d %v", code, obj)
	}
	if _, has := obj["warnings"]; has {
		t.Fatalf("warnings without cause: %v", obj)
	}

	code, obj = create(`,"model":"real"`)
	if code != 200 || obj["model"] != "real" {
		t.Fatalf("model: %d %v", code, obj)
	}
	if _, has := obj["warnings"]; has {
		t.Fatalf("warnings for an available model: %v", obj)
	}
	got := v.do("k", "GET", "/v1/batches/"+obj["id"].(string), "", nil)
	if gotObj := decode(t, got.Body.Bytes()); gotObj["model"] != "real" {
		t.Fatalf("get: %s", got.Body)
	}

	code, obj = create(`,"model":"held"`)
	warn, _ := json.Marshal(obj["warnings"])
	if code != 200 || obj["model"] != "held" || !strings.HasPrefix(string(warn), `[{"code":"model_sleep_held","message":"`) || !strings.HasSuffix(string(warn), `","param":"model"}]`) {
		t.Fatalf("held: %d %v", code, obj)
	}
	again := decode(t, v.do("k", "GET", "/v1/batches/"+obj["id"].(string), "", nil).Body.Bytes())
	if _, has := again["warnings"]; has || again["model"] != "held" {
		t.Fatalf("get after create: %v", again)
	}
	list := v.do("k", "GET", "/v1/batches", "", nil)
	if strings.Contains(list.Body.String(), "warnings") {
		t.Fatalf("list carries warnings: %s", list.Body)
	}

	rec := v.do("k", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h","model":"ghost"}`, file)))
	if want := `{"error":{"message":"No active deployment serves the model ghost","type":"invalid_request_error","param":"model","code":"model_not_found"}}`; rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body)
	}
}

// The console's multipart form: the model field in either order with the
// file; an unknown model leaves no file behind.
func TestConsoleFormModel(t *testing.T) {
	r := newRig(t, Config{Models: platformModels}, echo)
	r.start()
	form := func(modelFirst bool, model string) *multipart.Reader {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		writeModel := func() {
			if model != "" {
				_ = mw.WriteField("model", " "+model+" ")
			}
		}
		if modelFirst {
			writeModel()
		}
		_ = mw.WriteField("endpoint", EndpointChat)
		w, _ := mw.CreateFormFile("file", "in.jsonl")
		_, _ = w.Write([]byte(inputLines(1)))
		if !modelFirst {
			writeModel()
		}
		_ = mw.Close()
		return multipart.NewReader(&buf, mw.Boundary())
	}
	for _, first := range []bool{true, false} {
		b, err := r.svc.CreateFromUpload(testOwner, form(first, "real"))
		if err != nil || b.Model != "real" {
			t.Fatalf("model first=%v: %+v err=%v", first, b, err)
		}
	}
	b, err := r.svc.CreateFromUpload(testOwner, form(true, ""))
	if err != nil || b.Model != "" {
		t.Fatalf("no model: %+v err=%v", b, err)
	}
	b, err = r.svc.CreateFromUpload(testOwner, form(false, "held"))
	if err != nil || len(b.Warnings) != 1 {
		t.Fatalf("held: %+v err=%v", b, err)
	}
	files := len(r.repo.files)
	_, err = r.svc.CreateFromUpload(testOwner, form(false, "ghost"))
	var e *Error
	if !errors.As(err, &e) || e.Code != "model_not_found" || e.Model != "ghost" {
		t.Fatalf("unknown: %v", err)
	}
	if len(r.repo.files) != files {
		t.Fatalf("the refused upload left a file: %d, was %d", len(r.repo.files), files)
	}
}
