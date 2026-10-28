package aihelper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probeEndpoint answers the way the endpoints in the bug report answered: the
// body of each failure is the one recorded there, so a probe that passes here
// read the real answer.
func probeEndpoint(t *testing.T, model string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			// The gateway's answer for a path it does not serve.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"Not Found"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-right" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"detail":"invalid or missing API key"}`)
			return
		}
		var body ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"detail":"malformed request"}`)
			return
		}
		if body.Model != model {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"detail":"no deployment serves model %q"}`, body.Model)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func probe(t *testing.T, base, key, model string) ProbeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return NewClient(Endpoint{BaseURL: base, APIKey: key, Model: model}, "127.0.0.1").Probe(ctx)
}

// Each answer an operator can get is its own outcome: a wrong address, a wrong
// key and a model the endpoint does not serve all fail, and the fix differs.
func TestProbeClassifiesEachAnswer(t *testing.T) {
	srv := probeEndpoint(t, "served-model")
	base := srv.URL + "/v1"
	for _, tc := range []struct {
		name    string
		base    string
		key     string
		model   string
		outcome ProbeOutcome
		status  int
	}{
		{"a served model answers", base, "sk-right", "served-model", ProbeOK, 200},
		{"a wrong key is an auth failure", base, "sk-wrong", "served-model", ProbeAuth, 401},
		{"no key at all is an auth failure", base, "", "served-model", ProbeAuth, 401},
		// The call path appended twice is what the bug report recorded. The
		// probe now repairs it rather than reporting it: a row saved before
		// normalization existed must still be testable, and the operator then
		// sees the endpoint work instead of a 404 they did not cause.
		{"a pasted call path is repaired, not a wrong address", base + "/chat/completions", "sk-right", "served-model", ProbeOK, 200},
		{"a path the endpoint does not serve is a wrong address", base + "/v1", "sk-right", "served-model", ProbePath, 404},
		{"a host that serves no path is a wrong address", srv.URL, "sk-right", "served-model", ProbePath, 404},
		{"a model the endpoint does not serve is its own failure", base, "sk-right", "missing-model", ProbeModel, 404},
		{"a refused connection is a connection failure", "http://127.0.0.1:1/v1", "sk-right", "served-model", ProbeConnection, 0},
	} {
		got := probe(t, tc.base, tc.key, tc.model)
		if got.Outcome != tc.outcome || got.Status != tc.status {
			t.Errorf("%s: outcome=%s status=%d detail=%q; want %s %d", tc.name, got.Outcome, got.Status, got.Detail, tc.outcome, tc.status)
		}
	}
}

// An endpoint that answers 200 with something other than a completion is not
// working, and must not be reported as one.
func TestProbeRejectsNonCompletionSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>welcome</body></html>`)
	}))
	defer srv.Close()
	if got := probe(t, srv.URL, "sk-right", "m"); got.Outcome != ProbeHTTP || got.Status != http.StatusOK {
		t.Fatalf("outcome=%s status=%d", got.Outcome, got.Status)
	}
}

// A model the endpoint does not serve is reported as 400 by some servers: it
// is still the model, not a general failure.
func TestProbeReadsModelFailureFromAnyStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "{\"error\":{\"message\":\"The model `gpt-9` does not exist\",\"code\":\"model_not_found\"}}")
	}))
	defer srv.Close()
	if got := probe(t, srv.URL, "sk-right", "gpt-9"); got.Outcome != ProbeModel || got.Status != http.StatusBadRequest {
		t.Fatalf("outcome=%s status=%d detail=%q", got.Outcome, got.Status, got.Detail)
	}
}

// The address policy that keeps the assistant inside the platform applies to a
// test too, and its refusal is its own outcome: the operator fixes it by
// allowing the host, not by changing the provider.
func TestProbeReportsBlockedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := NewClient(Endpoint{BaseURL: srv.URL, APIKey: "sk-right", Model: "m"}).Probe(ctx)
	if got.Outcome != ProbeBlocked {
		t.Fatalf("outcome=%s detail=%q", got.Outcome, got.Detail)
	}
}

// A probe reports its own deadline rather than the assistant's long one, so a
// wrong address answers while the operator still watches the dialog.
func TestProbeStopsAtItsContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := NewClient(Endpoint{BaseURL: srv.URL, APIKey: "sk-right", Model: "m"}, "127.0.0.1").Probe(ctx)
	if time.Since(started) > 5*time.Second {
		t.Fatalf("the probe waited %s for a dead endpoint", time.Since(started))
	}
	if got.Outcome != ProbeConnection {
		t.Fatalf("outcome=%s detail=%q", got.Outcome, got.Detail)
	}
}

// Nothing a probe reports may carry the key: an endpoint can echo the request
// back in its error, and that text reaches the console.
func TestProbeNeverReturnsTheKey(t *testing.T) {
	const key = "sk-must-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"detail":"invalid API key %q in header Authorization: Bearer %s"}`, key, key)
	}))
	defer srv.Close()
	got := probe(t, srv.URL, key, "m")
	if strings.Contains(got.Detail, key) {
		t.Fatalf("the probe echoed the key: %q", got.Detail)
	}
	if got.Outcome != ProbeAuth {
		t.Fatalf("outcome=%s", got.Outcome)
	}
}

// An endpoint's error body is shortened to something a person reads, never a
// whole page.
func TestProbeShortensTheEndpointsWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, strings.Repeat("stack line\n", 500))
	}))
	defer srv.Close()
	got := probe(t, srv.URL, "sk-right", "m")
	if len([]rune(got.Detail)) > probeDetailRunes {
		t.Fatalf("detail is %d runes", len([]rune(got.Detail)))
	}
	if strings.Contains(got.Detail, "\n") {
		t.Fatalf("detail spans lines: %q", got.Detail)
	}
}

// A probe stores nothing and changes nothing: it is a call, not a write.
func TestProbeSendsTheCallPathOnce(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`)
	}))
	defer srv.Close()
	if got := probe(t, srv.URL, "sk-right", "m"); got.Outcome != ProbeOK {
		t.Fatalf("outcome=%s", got.Outcome)
	}
	if len(paths) != 1 || paths[0] != "POST /chat/completions" {
		t.Fatalf("calls = %v", paths)
	}
}
