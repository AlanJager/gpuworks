//go:build multiproc

package l2

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/integrationtest/fleet"
)

// gatewayTokenizeIsForwardedButNotTraffic runs inside
// TestProcessGatewayRequestTracesReachTheCollector, on its deployment: the
// shipped server forwards /v1/tokenize and /v1/detokenize to the fake
// engine's root paths under the engine's own model name (the fixture refuses
// any other with vLLM's 404), sends no span and leaves no request record;
// PUT /deployments/{id}/tokenize closes them and opens them again.
func gatewayTokenizeIsForwardedButNotTraffic(t *testing.T, base, token, apiKey string, client *fleet.Client, deploymentID uint, collector *otlpCollector, recorded int) {
	t.Helper()
	call := func(path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := processHTTPClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(bytes.TrimSpace(data))
	}
	spans := collector.count()
	status, body := call("/v1/tokenize", `{"model":"trace-model","prompt":"hi"}`)
	if status != http.StatusOK || body != `{"count":2,"max_model_len":40960,"tokens":[104,105],"token_strs":null}` {
		t.Fatalf("tokenize through the gateway = %d %s", status, body)
	}
	status, body = call("/v1/detokenize", `{"model":"trace-model","tokens":[104,105]}`)
	if status != http.StatusOK || body != `{"prompt":"hi"}` {
		t.Fatalf("detokenize through the gateway = %d %s", status, body)
	}
	status, body = call("/v1/tokenize", `{"model":"trace-model"}`)
	if status != http.StatusBadRequest || !bytes.Contains([]byte(body), []byte(`"param":"body.prompt"`)) {
		t.Fatalf("the engine's own validation = %d %s", status, body)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/tokenize", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET /v1/tokenize = %d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}

	// Closed: the gateway answers 404 itself once its route table has the
	// change; opened again, tokenize is back.
	put := func(enabled bool) {
		t.Helper()
		resp, data := authedDo(t, http.MethodPut, base, token, fmt.Sprintf("/api/v1/deployments/%d/tokenize", deploymentID), []byte(fmt.Sprintf(`{"enabled":%t}`, enabled)))
		if resp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(fmt.Sprintf(`"tokenize_enabled":%t`, enabled))) {
			t.Fatalf("PUT tokenize %t = %d %s", enabled, resp.StatusCode, data)
		}
	}
	until := func(want int) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			status, body := call("/v1/tokenize", `{"model":"trace-model","prompt":"hi"}`)
			if status == want {
				return body
			}
			if time.Now().After(deadline) {
				t.Fatalf("tokenize = %d %s, want %d", status, body, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	put(false)
	if body := until(http.StatusNotFound); !bytes.Contains([]byte(body), []byte("tokenize is turned off")) {
		t.Fatalf("closed = %s", body)
	}
	put(true)
	until(http.StatusOK)

	// None of it is traffic: no span and, after the request store's flush, no
	// record beyond the chat requests already there.
	time.Sleep(3 * time.Second)
	if collector.count() != spans {
		t.Fatalf("tokenize sent spans: %d -> %d", spans, collector.count())
	}
	var history struct {
		Requests []struct {
			Path string `json:"path"`
		} `json:"requests"`
	}
	if err := client.GetJSON(fmt.Sprintf("/api/v1/monitor/deployments/%d/requests?view=history&range=1h", deploymentID), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Requests) != recorded {
		t.Fatalf("tokenize left request records: %d -> %d %+v", recorded, len(history.Requests), history.Requests)
	}
}
