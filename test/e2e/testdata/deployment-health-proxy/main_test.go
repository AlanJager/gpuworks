package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `{"model":"served","messages":[{"role":"user","content":"1"}],"max_tokens":1,"stream":false}`

func setup(t *testing.T, configuration string) (string, *audit, func() []event) {
	t.Helper()
	dir := t.TempDir()
	control := filepath.Join(dir, "control.json")
	if err := os.WriteFile(control, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	a := &audit{file: file}
	read := func() []event {
		raw, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		var out []event
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		for {
			var entry event
			err = decoder.Decode(&entry)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, entry)
		}
		return out
	}
	return control, a, read
}
func TestRejectWrongPayloads(t *testing.T) {
	for _, raw := range []string{`{}`, strings.Replace(good, `"max_tokens":1`, `"max_tokens":"1"`, 1), strings.Replace(good, `"stream":false`, `"stream":true`, 1), strings.Replace(good, `"1"`, `"wrong"`, 1), strings.TrimSuffix(good, "}") + `,"extra":true}`, good + good, strings.Replace(good, `"max_tokens":1`, `"max_tokens":1.0`, 1)} {
		if _, err := payload([]byte(raw), "served"); err == nil {
			t.Fatalf("accepted bad body %s", raw)
		}
	}
	if _, err := payload([]byte(good), "served"); err != nil {
		t.Fatal(err)
	}
}
func TestControlRejectsMalformedAndUnknown(t *testing.T) {
	for _, configuration := range []string{`{}`, `{"mode":"maybe"}`, `{"mode":"slow","delay_ms":0}`, `{"mode":"ok","delay_ms":-1}`, `{"mode":"ok","unknown":1}`, `{"mode":"ok"} {}`} {
		path, _, _ := setup(t, configuration)
		if _, err := readControl(path); err == nil {
			t.Fatalf("accepted control %s", configuration)
		}
	}
}
func TestActualForwardAndOrderedAudit(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != good || r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			t.Error("forwarded request drift")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"real":"child"}`)
	}))
	defer upstream.Close()
	control, a, read := setup(t, `{"mode":"ok","delay_ms":0}`)
	response := httptest.NewRecorder()
	handler(upstream.URL, "served", control, a).ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(good)))
	if response.Code != 200 || response.Body.String() != `{"real":"child"}` || upstreamCalls != 1 {
		t.Fatal("upstream result was not preserved")
	}
	entries := read()
	if len(entries) != 2 || entries[0].Seq != 1 || entries[0].Phase != "started" || entries[1].Seq != 2 || entries[1].Request != 1 || entries[1].Phase != "completed" || entries[1].HTTP != 200 {
		t.Fatalf("audit %#v", entries)
	}
}
func TestSlowCancellationHasNoForwardAndTerminalAudit(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer upstream.Close()
	control, a, read := setup(t, `{"mode":"slow","delay_ms":2000}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(good)).WithContext(ctx)
	handler(upstream.URL, "served", control, a).ServeHTTP(httptest.NewRecorder(), request)
	entries := read()
	if calls != 0 || len(entries) != 2 || entries[1].Phase != "cancelled" || entries[1].Request != 1 {
		t.Fatalf("cancel audit %#v /calls%d", entries, calls)
	}
}
func TestFailureAndMethodValidation(t *testing.T) {
	control, a, read := setup(t, `{"mode":"fail","delay_ms":0}`)
	h := handler("http://127.0.0.1:1", "served", control, a)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(good)))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "e2e-health-controlled-failure") {
		t.Fatal("wrong controlled failure")
	}
	for _, method := range []string{"GET", "PUT"} {
		response = httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(method, "/v1/chat/completions", strings.NewReader(good)))
		if response.Code != 400 {
			t.Fatal("wrong method accepted")
		}
	}
	entries := read()
	if entries[1].HTTP != 503 || entries[2].Phase != "rejected" || entries[3].Phase != "rejected" {
		t.Fatalf("failure audit %#v", entries)
	}
}
func TestControlFailureAndTransportFailureCannotSucceed(t *testing.T) {
	for _, configuration := range []string{`{"mode":"unknown"}`, `{"mode":"ok"}`} {
		control, a, read := setup(t, configuration)
		response := httptest.NewRecorder()
		handler("http://127.0.0.1:1", "served", control, a).ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(good)))
		entries := read()
		if response.Code != 500 && response.Code != 502 {
			t.Fatal("failure became success")
		}
		if entries[len(entries)-1].Phase != "control_failed" && entries[len(entries)-1].Phase != "transport_failed" {
			t.Fatalf("failure missing %#v", entries)
		}
	}
}
