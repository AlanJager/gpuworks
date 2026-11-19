package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// omniSpeechUpstream answers as vllm-omni's speech route does: only POST
// /v1/audio/speech exists, the body is validated before the model is looked
// up (FastAPI's 422 for a missing input), a model the server was not started
// with is vLLM's 404, and the reply is the audio itself, not JSON.
func omniSpeechUpstream(served string, wav []byte, seen *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/v1/audio/speech" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Model          string  `json:"model"`
			Input          *string `json:"input"`
			ResponseFormat string  `json:"response_format"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Input == nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"detail":[{"type":"missing","loc":["body","input"],"msg":"Field required"}]}`)
			return
		}
		if body.Model != served {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"The model `+"`"+body.Model+"`"+` does not exist.","type":"NotFoundError","param":"model","code":404}}`)
			return
		}
		if body.ResponseFormat != "wav" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected response_format","type":"BadRequestError","param":"response_format","code":400}}`)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	})
}

// A speech request for a running vLLM deployment launched with --omni is
// routed like any other: the gateway picks instances by their state, sleep,
// port and node connection, never by the operation asked for, and hands the
// audio back as the engine sent it.
func TestSpeechRequestReachesTheRunningInstance(t *testing.T) {
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0x00, 0x7f, 0xff}, 64)...)
	var seen []string
	p, apiKey, closeUpstream := newProxyE2E(t, omniSpeechUpstream("omni", wav, &seen))
	defer closeUpstream()

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		p.Handler().ServeHTTP(w, req)
		return w
	}

	w := post(`{"model":"omni","input":"Hello","response_format":"wav"}`)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "audio/wav" || !bytes.Equal(w.Body.Bytes(), wav) {
		t.Fatalf("speech reply = %d %q, %d bytes (want 200 audio/wav, %d bytes): %s", w.Code, w.Header().Get("Content-Type"), w.Body.Len(), len(wav), w.Body.String())
	}
	if len(seen) != 1 || seen[0] != "POST /v1/audio/speech" {
		t.Fatalf("upstream saw %v, want one POST /v1/audio/speech", seen)
	}

	// The engine's own refusal comes back as the engine's, not as a routing
	// refusal of the gateway's.
	w = post(`{"model":"omni","response_format":"wav"}`)
	if w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "MODEL_NOT_READY") {
		t.Fatalf("engine validation reply = %d %s", w.Code, w.Body.String())
	}

	// A name no deployment serves is not found, not "not ready".
	w = post(`{"model":"qa-n-45","input":"Hello","response_format":"wav"}`)
	var refusal struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("refusal body: %v: %s", err, w.Body.String())
	}
	if w.Code != http.StatusNotFound || refusal.Error.Code != "MODEL_NOT_FOUND" {
		t.Fatalf("unknown model reply = %d %s", w.Code, w.Body.String())
	}
}
