package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// v1Rig serves the /v1 handler over a rig: keys "k1" and "k2" belong to
// users 1 and 2, "k1b" is a second key of user 1.
type v1Rig struct {
	*rig
	h http.Handler
}

func newV1Rig(t *testing.T) *v1Rig {
	t.Helper()
	r := newRig(t, Config{}, echo)
	r.start()
	keys := map[string]Owner{"k1": {UserID: 1, APIKeyID: 11}, "k1b": {UserID: 1, APIKeyID: 12}, "k2": {UserID: 2, APIKeyID: 21}}
	return &v1Rig{rig: r, h: r.svc.V1Handler(func(req *http.Request) (Owner, bool) {
		owner, ok := keys[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
		return owner, ok
	})}
}

func (v *v1Rig) do(key, method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	v.h.ServeHTTP(rec, req)
	return rec
}

func (v *v1Rig) upload(key, content string, fields ...[2]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if f[0] == "file" {
			w, _ := mw.CreateFormFile("file", f[1])
			io.WriteString(w, content)
		} else {
			mw.WriteField(f[0], f[1])
		}
	}
	mw.Close()
	return v.do(key, "POST", "/v1/files", mw.FormDataContentType(), &buf)
}

func (v *v1Rig) uploadBatchFile(key, content string) map[string]any {
	v.t.Helper()
	rec := v.upload(key, content, [2]string{"purpose", "batch"}, [2]string{"file", "in.jsonl"})
	if rec.Code != 200 {
		v.t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	return decode(v.t, rec.Body.Bytes())
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return out
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The /v1 routes are exactly the documented set.
func TestV1RouteInventory(t *testing.T) {
	v := newV1Rig(t)
	var got []string
	if err := chi.Walk(v.h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !slices.Equal(got, V1Routes) {
		t.Fatalf("routes changed; update the contract deliberately\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(V1Routes, "\n"))
	}
}

func TestV1RoutesRequireAnAPIKeyAndRefuseOtherMethods(t *testing.T) {
	v := newV1Rig(t)
	for _, route := range V1Routes {
		parts := strings.SplitN(route, " ", 2)
		path := strings.ReplaceAll(parts[1], "{id}", "file-000000000000000000000000")
		for _, key := range []string{"", "wrong"} {
			rec := v.do(key, parts[0], path, "", nil)
			if rec.Code != 401 {
				t.Errorf("%s with key %q = %d", route, key, rec.Code)
			}
			if got := rec.Body.String(); got != `{"error":{"message":"invalid or missing API key","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`+"\n" {
				t.Errorf("%s 401 body = %s", route, got)
			}
		}
	}
	if rec := v.do("k1", "PATCH", "/v1/batches", "", nil); rec.Code != 405 {
		t.Errorf("PATCH /v1/batches = %d", rec.Code)
	}
	if rec := v.do("k1", "GET", "/v1/nothing", "", nil); rec.Code != 404 {
		t.Errorf("unknown path = %d", rec.Code)
	}
}

func TestFileObjectWireJSON(t *testing.T) {
	v := newV1Rig(t)
	in := inputLines(3)
	rec := v.upload("k1", in, [2]string{"purpose", "batch"}, [2]string{"file", "my reqs.jsonl"})
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("upload = %d %s", rec.Code, rec.Header())
	}
	obj := decode(t, rec.Body.Bytes())
	id := obj["id"].(string)
	if !strings.HasPrefix(id, "file-") || len(id) != 29 {
		t.Fatalf("id = %q", id)
	}
	created := int64(obj["created_at"].(float64))
	if d := v.clock.Now().Unix() - created; d != 0 {
		t.Fatalf("created_at = %d", created)
	}
	want := fmt.Sprintf(`{"id":%q,"object":"file","bytes":%d,"created_at":%d,"filename":"my reqs.jsonl","purpose":"batch","status":"processed"}`, id, len(in), created)
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("file wire JSON\n got %s\nwant %s", got, want)
	}
	if got := v.do("k1", "GET", "/v1/files/"+id, "", nil); strings.TrimSpace(got.Body.String()) != want {
		t.Fatalf("GET file = %s", got.Body)
	}
	content := v.do("k1", "GET", "/v1/files/"+id+"/content", "", nil)
	if content.Code != 200 || content.Body.String() != in || content.Header().Get("Content-Length") != fmt.Sprint(len(in)) {
		t.Fatalf("content = %d %q", content.Code, content.Body.String())
	}
}

func TestUploadAcceptsThePartsInEitherOrderAndRefusesOtherPurposes(t *testing.T) {
	v := newV1Rig(t)
	if rec := v.upload("k1", inputLines(1), [2]string{"file", "a.jsonl"}, [2]string{"purpose", "batch"}); rec.Code != 200 {
		t.Fatalf("file before purpose = %d %s", rec.Code, rec.Body)
	}
	for name, fields := range map[string][][2]string{
		"fine-tune":       {{"purpose", "fine-tune"}, {"file", "a.jsonl"}},
		"fine-tune later": {{"file", "a.jsonl"}, {"purpose", "fine-tune"}},
		"no purpose":      {{"file", "a.jsonl"}},
	} {
		rec := v.upload("k1", inputLines(1), fields...)
		if rec.Code != 400 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := v.upload("k1", "", [2]string{"purpose", "batch"}); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"param":"file"`) {
		t.Errorf("no file = %d %s", rec.Code, rec.Body)
	}
	if rec := v.do("k1", "POST", "/v1/files", "application/json", strings.NewReader("{}")); rec.Code != 400 {
		t.Errorf("json upload = %d", rec.Code)
	}
	files := decode(t, v.do("k1", "GET", "/v1/files", "", nil).Body.Bytes())
	if n := len(files["data"].([]any)); n != 1 {
		t.Fatalf("%d files stored; refused uploads must leave nothing (and the wrong-purpose ones are removed)", n)
	}
	entries := len(globFiles(t, v.cfg.Dir))
	if entries != 1 {
		t.Fatalf("%d files on disk, want 1", entries)
	}
}

func TestUploadValidationErrorsNameTheLine(t *testing.T) {
	v := newV1Rig(t)
	good := func(i int) string {
		return fmt.Sprintf(`{"custom_id":"r%d","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[]}}`, i)
	}
	for _, tc := range []struct {
		name, content, code, param string
		line                       int
		status                     int
	}{
		{"not json", good(1) + "\n" + "{oops\n", "invalid_json_line", "", 2, 400},
		{"not an object", good(1) + "\n[1,2]\n", "invalid_json_line", "", 2, 400},
		{"missing custom_id", good(1) + "\n" + `{"method":"POST","url":"/v1/chat/completions","body":{"model":"m"}}` + "\n", "missing_custom_id", "custom_id", 2, 400},
		{"numeric custom_id", `{"custom_id":5,"method":"POST","url":"/v1/chat/completions","body":{"model":"m"}}` + "\n", "missing_custom_id", "custom_id", 1, 400},
		{"empty custom_id", `{"custom_id":"","method":"POST","url":"/v1/chat/completions","body":{"model":"m"}}` + "\n", "missing_custom_id", "custom_id", 1, 400},
		{"duplicate custom_id", good(1) + "\n" + good(2) + "\n" + good(1) + "\n", "duplicate_custom_id", "custom_id", 3, 400},
		{"method", `{"custom_id":"a","method":"GET","url":"/v1/chat/completions","body":{"model":"m"}}` + "\n", "invalid_method", "method", 1, 400},
		{"url", good(1) + "\n" + `{"custom_id":"a","method":"POST","url":"/v1/images","body":{"model":"m"}}` + "\n", "invalid_url", "url", 2, 400},
		{"no body", `{"custom_id":"a","method":"POST","url":"/v1/embeddings"}` + "\n", "missing_body", "body", 1, 400},
		{"body not object", `{"custom_id":"a","method":"POST","url":"/v1/embeddings","body":"x"}` + "\n", "missing_body", "body", 1, 400},
		{"no model", `{"custom_id":"a","method":"POST","url":"/v1/embeddings","body":{"input":"x"}}` + "\n", "missing_model", "body.model", 1, 400},
		{"stream", `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"m","stream":true}}` + "\n", "streaming_not_supported", "body.stream", 1, 400},
		{"empty", "\n\n", "empty_file", "", 0, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := v.upload("k1", tc.content, [2]string{"purpose", "batch"}, [2]string{"file", "in.jsonl"})
			if rec.Code != tc.status {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			var body struct {
				Error struct {
					Message string  `json:"message"`
					Type    string  `json:"type"`
					Param   *string `json:"param"`
					Code    string  `json:"code"`
					Line    int     `json:"line"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			e := body.Error
			if e.Type != "invalid_request_error" || e.Code != tc.code || e.Line != tc.line || (tc.param == "") != (e.Param == nil) || (e.Param != nil && *e.Param != tc.param) {
				t.Fatalf("error = %+v", e)
			}
			if tc.line > 0 && !strings.HasPrefix(e.Message, fmt.Sprintf("Line %d: ", tc.line)) {
				t.Fatalf("message = %q", e.Message)
			}
		})
	}
	if n := len(globFiles(t, v.cfg.Dir)); n != 0 {
		t.Fatalf("%d files left on disk by refused uploads", n)
	}
}

func TestUploadLimits(t *testing.T) {
	v := newV1Rig(t)
	var big strings.Builder
	line := `{"custom_id":"%d","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}` + "\n"
	for i := 0; i <= MaxFileLines; i++ {
		fmt.Fprintf(&big, line, i)
	}
	rec := v.upload("k1", big.String(), [2]string{"purpose", "batch"}, [2]string{"file", "in.jsonl"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"too_many_requests"`) || !strings.Contains(rec.Body.String(), `"line":50001`) {
		t.Fatalf("too many lines = %d %.200s", rec.Code, rec.Body)
	}
	one := fmt.Sprintf(line, 1)
	svc := v.svc
	defer func(old int64) { fileLimit = old }(fileLimit)
	fileLimit = 1 << 20
	if _, err := svc.Upload(testOwner, "a.jsonl", io.MultiReader(strings.NewReader(one), &zeroReader{n: fileLimit})); err == nil {
		t.Fatal("a file over 200 MB was accepted")
	} else if e := err.(*Error); e.Status != 413 || e.Code != "file_too_large" || !strings.Contains(e.Message, "1 MB") {
		t.Fatalf("error = %+v", e)
	}
	if n := len(globFiles(t, v.cfg.Dir)); n != 0 {
		t.Fatalf("%d files left behind", n)
	}
	// A file of exactly the limit is fine.
	if _, err := svc.Upload(testOwner, "a.jsonl", io.MultiReader(strings.NewReader(one), &zeroReader{n: fileLimit - int64(len(one))})); err != nil {
		t.Fatalf("a file at the limit: %v", err)
	}
}

// zeroReader yields n newlines.
type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = '\n'
	}
	z.n -= int64(len(p))
	return len(p), nil
}

func globFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := readDirNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestBatchObjectWireJSONAndLifecycle(t *testing.T) {
	v := newV1Rig(t)
	gate := make(chan struct{})
	v.send.handle = func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return okReply(`{}`)
	}
	file := v.uploadBatchFile("k1", inputLines(2))
	body := fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"team":"nlp"}}`, file["id"])
	rec := v.do("k1", "POST", "/v1/batches", "application/json", strings.NewReader(body))
	if rec.Code != 200 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	obj := decode(t, rec.Body.Bytes())
	wantKeys := []string{"cancelled_at", "cancelling_at", "completed_at", "completion_window", "created_at", "endpoint", "error_file_id", "errors", "expired_at", "expires_at", "failed_at", "finalizing_at", "id", "in_progress_at", "input_file_id", "metadata", "object", "output_file_id", "request_counts", "status"}
	if got := keysOf(obj); !slices.Equal(got, wantKeys) {
		t.Fatalf("batch keys = %v\nwant %v", got, wantKeys)
	}
	id := obj["id"].(string)
	created := int64(obj["created_at"].(float64))
	want := fmt.Sprintf(`{"id":%q,"object":"batch","endpoint":"/v1/chat/completions","errors":null,"input_file_id":%q,"completion_window":"24h","status":"validating","output_file_id":null,"error_file_id":null,"created_at":%d,"in_progress_at":null,"expires_at":%d,"finalizing_at":null,"completed_at":null,"failed_at":null,"expired_at":null,"cancelling_at":null,"cancelled_at":null,"request_counts":{"total":2,"completed":0,"failed":0},"metadata":{"team":"nlp"}}`,
		id, file["id"], created, created+86400)
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("batch wire JSON\n got %s\nwant %s", got, want)
	}
	if !strings.HasPrefix(id, "batch_") {
		t.Fatalf("id = %q", id)
	}

	// In progress, then the input file is protected from deletion.
	v.await(id, StatusInProgress)
	if rec := v.do("k1", "DELETE", "/v1/files/"+file["id"].(string), "", nil); rec.Code != 409 || !strings.Contains(rec.Body.String(), `"code":"file_in_use"`) {
		t.Fatalf("delete in use = %d %s", rec.Code, rec.Body)
	}
	cancel := v.do("k1", "POST", "/v1/batches/"+id+"/cancel", "", nil)
	cobj := decode(t, cancel.Body.Bytes())
	if cancel.Code != 200 || cobj["status"] != "cancelling" || cobj["cancelling_at"] == nil {
		t.Fatalf("cancel = %d %s", cancel.Code, cancel.Body)
	}
	close(gate)
	v.await(id, StatusCancelled)
	done := decode(t, v.do("k1", "GET", "/v1/batches/"+id, "", nil).Body.Bytes())
	if done["status"] != "cancelled" || done["cancelled_at"] == nil || done["finalizing_at"] != nil {
		t.Fatalf("cancelled batch = %v", done)
	}
	if again := v.do("k1", "POST", "/v1/batches/"+id+"/cancel", "", nil); again.Code != 409 || !strings.Contains(again.Body.String(), `"code":"batch_not_cancellable"`) {
		t.Fatalf("cancel again = %d %s", again.Code, again.Body)
	}
	// Once the batch has ended, the input file can go.
	del := v.do("k1", "DELETE", "/v1/files/"+file["id"].(string), "", nil)
	if del.Code != 200 || strings.TrimSpace(del.Body.String()) != fmt.Sprintf(`{"id":%q,"object":"file","deleted":true}`, file["id"]) {
		t.Fatalf("delete = %d %s", del.Code, del.Body)
	}
	if rec := v.do("k1", "GET", "/v1/files/"+file["id"].(string), "", nil); rec.Code != 404 {
		t.Fatalf("deleted file = %d", rec.Code)
	}
}

func TestCompletedBatchWireAndOutputFile(t *testing.T) {
	v := newV1Rig(t)
	v.send.handle = func(_ context.Context, _ Owner, _ string, body []byte) Reply {
		if strings.Contains(string(body), `"q1"`) {
			return Reply{Status: 400, RequestID: "rq", Body: []byte(`{"error":{"message":"bad prompt","code":"bad"}}`)}
		}
		return okReply(`{"id":"cmpl-1"}`)
	}
	file := v.uploadBatchFile("k1", inputLines(3))
	rec := v.do("k1", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file["id"])))
	id := decode(t, rec.Body.Bytes())["id"].(string)
	v.await(id, StatusCompleted)
	obj := decode(t, v.do("k1", "GET", "/v1/batches/"+id, "", nil).Body.Bytes())
	counts := obj["request_counts"].(map[string]any)
	if counts["total"] != 3.0 || counts["completed"] != 2.0 || counts["failed"] != 1.0 || obj["output_file_id"] == nil || obj["error_file_id"] == nil || obj["metadata"] != nil || obj["errors"] != nil {
		t.Fatalf("completed batch = %v", obj)
	}
	out := v.do("k1", "GET", "/v1/files/"+obj["output_file_id"].(string)+"/content", "", nil)
	lines := strings.Split(strings.TrimSpace(out.Body.String()), "\n")
	if out.Code != 200 || len(lines) != 2 {
		t.Fatalf("output = %d %q", out.Code, out.Body)
	}
	first := decode(t, []byte(lines[0]))
	if got := keysOf(first); !slices.Equal(got, []string{"custom_id", "error", "id", "response"}) {
		t.Fatalf("output line keys = %v", got)
	}
	resp := first["response"].(map[string]any)
	if first["error"] != nil || resp["status_code"] != 200.0 || resp["request_id"] != "req-1" || fmt.Sprint(resp["body"]) != "map[id:cmpl-1]" {
		t.Fatalf("output line = %s", lines[0])
	}
	errOut := v.do("k1", "GET", "/v1/files/"+obj["error_file_id"].(string)+"/content", "", nil)
	want := fmt.Sprintf(`{"id":%q,"custom_id":"c1","response":{"status_code":400,"request_id":"rq","body":{"error":{"message":"bad prompt","code":"bad"}}},"error":{"code":"bad","message":"bad prompt"}}`, requestID(id, "c1"))
	if strings.TrimSpace(errOut.Body.String()) != want {
		t.Fatalf("error line\n got %s\nwant %s", errOut.Body, want)
	}
	file2 := decode(t, v.do("k1", "GET", "/v1/files/"+obj["output_file_id"].(string), "", nil).Body.Bytes())
	if file2["purpose"] != "batch_output" || file2["status"] != "processed" {
		t.Fatalf("output file object = %v", file2)
	}
}

func TestCreateBatchValidation(t *testing.T) {
	v := newV1Rig(t)
	chat := v.uploadBatchFile("k1", inputLines(1))["id"].(string)
	embed := v.uploadBatchFile("k1", `{"custom_id":"e","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}`+"\n")["id"].(string)
	mixed := v.uploadBatchFile("k1", inputLines(1)+`{"custom_id":"e","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}`+"\n")["id"].(string)
	foreign := v.uploadBatchFile("k2", inputLines(1))["id"].(string)
	req := func(key, body string) *httptest.ResponseRecorder {
		return v.do(key, "POST", "/v1/batches", "application/json", strings.NewReader(body))
	}
	manyMeta := map[string]string{}
	for i := 0; i < 17; i++ {
		manyMeta[fmt.Sprint("k", i)] = "v"
	}
	manyMetaJSON, _ := json.Marshal(manyMeta)
	for _, tc := range []struct {
		name, key, body, code, param string
		status                       int
	}{
		{"no input file", "k1", `{"endpoint":"/v1/chat/completions","completion_window":"24h"}`, "missing_required_parameter", "input_file_id", 400},
		{"unknown file", "k1", `{"input_file_id":"file-000000000000000000000000","endpoint":"/v1/chat/completions","completion_window":"24h"}`, "file_not_found", "input_file_id", 400},
		{"another user's file", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, foreign), "file_not_found", "input_file_id", 400},
		{"bad endpoint", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/images","completion_window":"24h"}`, chat), "invalid_endpoint", "endpoint", 400},
		{"endpoint mismatch", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/embeddings","completion_window":"24h"}`, chat), "endpoint_mismatch", "endpoint", 400},
		{"embeddings file as chat", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, embed), "endpoint_mismatch", "endpoint", 400},
		{"mixed urls", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, mixed), "endpoint_mismatch", "endpoint", 400},
		{"window 0h", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"0h"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window 73h", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"73h"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window one day", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"1d"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window capital", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24H"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window padded", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"024h"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window fraction", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"1.5h"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window minutes", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"90m"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"window bare number", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"no window", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions"}`, chat), "invalid_completion_window", "completion_window", 400},
		{"metadata count", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h","metadata":%s}`, chat, manyMetaJSON), "invalid_metadata", "metadata", 400},
		{"metadata value", "k1", fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"a":1}}`, chat), "invalid_json", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := req(tc.key, tc.body)
			e := decode(t, rec.Body.Bytes())["error"].(map[string]any)
			if rec.Code != tc.status || e["code"] != tc.code || e["type"] != "invalid_request_error" {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if tc.param != "" && e["param"] != tc.param {
				t.Fatalf("param = %v", e["param"])
			}
		})
	}
	if rec := req("k1", `not json`); rec.Code != 400 {
		t.Errorf("bad JSON = %d", rec.Code)
	}
}

// A user sees only the files and batches of its own user, whichever of its
// keys made them.
func TestOwnershipScopesFilesAndBatchesToTheUser(t *testing.T) {
	v := newV1Rig(t)
	file := v.uploadBatchFile("k1", inputLines(1))["id"].(string)
	rec := v.do("k1b", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file)))
	if rec.Code != 200 {
		t.Fatalf("a second key of the same user cannot use the file: %d %s", rec.Code, rec.Body)
	}
	id := decode(t, rec.Body.Bytes())["id"].(string)
	v.await(id, StatusCompleted)
	for _, path := range []string{"/v1/files/" + file, "/v1/files/" + file + "/content", "/v1/batches/" + id} {
		if rec := v.do("k2", "GET", path, "", nil); rec.Code != 404 {
			t.Errorf("another user reads %s: %d", path, rec.Code)
		}
		if rec := v.do("k1", "GET", path, "", nil); rec.Code != 200 {
			t.Errorf("owner reads %s: %d", path, rec.Code)
		}
	}
	if rec := v.do("k2", "POST", "/v1/batches/"+id+"/cancel", "", nil); rec.Code != 404 {
		t.Errorf("another user cancels = %d", rec.Code)
	}
	if rec := v.do("k2", "DELETE", "/v1/files/"+file, "", nil); rec.Code != 404 {
		t.Errorf("another user deletes = %d", rec.Code)
	}
	for _, path := range []string{"/v1/files", "/v1/batches"} {
		list := decode(t, v.do("k2", "GET", path, "", nil).Body.Bytes())
		if len(list["data"].([]any)) != 0 || list["first_id"] != nil || list["has_more"] != false {
			t.Errorf("another user lists %s: %v", path, list)
		}
	}
	admin := Owner{All: true}
	if _, err := v.svc.Batch(admin, id); err != nil {
		t.Errorf("the console's administrator cannot see it: %v", err)
	}
}

func TestListPaginationCursorsAndFilters(t *testing.T) {
	v := newV1Rig(t)
	file := v.uploadBatchFile("k1", inputLines(1))["id"].(string)
	var ids []string
	for i := 0; i < 5; i++ {
		v.clock.Advance(time.Second)
		rec := v.do("k1", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file)))
		ids = append(ids, decode(t, rec.Body.Bytes())["id"].(string))
	}
	for _, id := range ids {
		v.await(id, StatusCompleted)
	}
	list := func(q string) map[string]any {
		return decode(t, v.do("k1", "GET", "/v1/batches"+q, "", nil).Body.Bytes())
	}
	page1 := list("?limit=2")
	data := page1["data"].([]any)
	if page1["object"] != "list" || len(data) != 2 || page1["has_more"] != true || page1["first_id"] != ids[4] || page1["last_id"] != ids[3] {
		t.Fatalf("page 1 = %v", page1)
	}
	page2 := list("?limit=2&after=" + ids[3])
	if page2["first_id"] != ids[2] || page2["last_id"] != ids[1] || page2["has_more"] != true {
		t.Fatalf("page 2 = %v", page2)
	}
	page3 := list("?limit=2&after=" + ids[1])
	if page3["first_id"] != ids[0] || page3["last_id"] != ids[0] || page3["has_more"] != false || len(page3["data"].([]any)) != 1 {
		t.Fatalf("page 3 = %v", page3)
	}
	if def := list(""); len(def["data"].([]any)) != 5 {
		t.Fatalf("default list = %v", def)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?order=asc"} {
		if rec := v.do("k1", "GET", "/v1/batches"+q, "", nil); rec.Code != 400 {
			t.Errorf("GET /v1/batches%s = %d", q, rec.Code)
		}
	}
	files := decode(t, v.do("k1", "GET", "/v1/files?purpose=batch_output&limit=3", "", nil).Body.Bytes())
	if len(files["data"].([]any)) != 3 || files["has_more"] != true {
		t.Fatalf("output files = %v", files)
	}
	for _, f := range files["data"].([]any) {
		if f.(map[string]any)["purpose"] != "batch_output" {
			t.Fatalf("purpose filter ignored: %v", f)
		}
	}
	if rec := v.do("k1", "GET", "/v1/files?purpose=assistants", "", nil); rec.Code != 400 {
		t.Errorf("unknown purpose filter = %d", rec.Code)
	}
}

func TestOutputFilesExpireAfterTheRetention(t *testing.T) {
	v := newV1Rig(t)
	file := v.uploadBatchFile("k1", inputLines(1))["id"].(string)
	rec := v.do("k1", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file)))
	id := decode(t, rec.Body.Bytes())["id"].(string)
	done := v.await(id, StatusCompleted)
	out := done.OutputFileID
	v.clock.Advance(31 * 24 * time.Hour)
	v.svc.sweep()
	if rec := v.do("k1", "GET", "/v1/files/"+out+"/content", "", nil); rec.Code != 404 {
		t.Fatalf("expired output = %d", rec.Code)
	}
	if rec := v.do("k1", "GET", "/v1/files/"+file+"/content", "", nil); rec.Code != 200 {
		t.Fatalf("the input file is kept until the user deletes it: %d", rec.Code)
	}
	if got := globFiles(t, v.cfg.Dir); len(got) != 1 {
		t.Fatalf("files on disk = %v", got)
	}
}

// Only a .jsonl file is a batch input: the API names JSON Lines, and a
// .json or .txt file is refused before anything is written.
func TestUploadRefusesAFileThatIsNotJSONL(t *testing.T) {
	v := newV1Rig(t)
	line := `{"custom_id":"1","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}` + "\n"
	for _, name := range []string{"in.json", "in.txt", "in", "in.jsonl.txt"} {
		rec := v.upload("k1", line, [2]string{"purpose", "batch"}, [2]string{"file", name})
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"invalid_file_extension"`) {
			t.Fatalf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	for _, name := range []string{"in.jsonl", "IN.JSONL"} {
		if rec := v.upload("k1", line, [2]string{"purpose", "batch"}, [2]string{"file", name}); rec.Code != 200 {
			t.Fatalf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if n := len(globFiles(t, v.cfg.Dir)); n != 2 {
		t.Fatalf("%d files kept, want only the two accepted", n)
	}
}
