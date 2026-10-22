//go:build multiproc

package l2

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// otlpSpanWire is the part of an OTLP/JSON span this test reads.
type otlpSpanWire struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Attributes   []struct {
		Key   string `json:"key"`
		Value struct {
			String *string `json:"stringValue"`
			Int    *string `json:"intValue"`
		} `json:"value"`
	} `json:"attributes"`
}

func (s otlpSpanWire) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			switch {
			case a.Value.String != nil:
				return *a.Value.String
			case a.Value.Int != nil:
				return *a.Value.Int
			}
		}
	}
	return ""
}

// otlpCollector is an OTLP/HTTP receiver in the test process: the server
// binary posts to it like it would to an OpenTelemetry Collector.
type otlpCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	spans  []otlpSpanWire
	auth   []string
}

func newOTLPCollector(t *testing.T) *otlpCollector {
	c := &otlpCollector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "not an OTLP/HTTP JSON export", http.StatusBadRequest)
			return
		}
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body = zr
		}
		var req struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []otlpSpanWire `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.mu.Unlock()
		_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *otlpCollector) span(traceID string) (otlpSpanWire, []string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.spans {
		if s.TraceID == traceID {
			return s, append([]string(nil), c.auth...), true
		}
	}
	return otlpSpanWire{}, nil, false
}

func (c *otlpCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.spans)
}

// TestProcessGatewayRequestTracesReachTheCollector drives the shipped server
// binary and a real agent (fleet-host, one node, one fake engine): with the
// trace export configured through the API, a chat request that continues a
// caller's trace arrives at an OTLP collector as a child of the caller's
// span, carrying the request's facts; the request view and the lookup find
// it by its trace id; a request without a trace starts a time-ordered one
// the lookup finds without a window; turning export off stops the spans.
//
//	go test -count=1 -v -tags=multiproc -timeout 20m ./test/l2/ -run '^TestProcessGatewayRequestTracesReachTheCollector$'
func TestProcessGatewayRequestTracesReachTheCollector(t *testing.T) {
	serverBin := fleetBinary(t, "GPUWORKS_FLEET_SERVER_BIN", "./cmd/gpuworks")
	hostBin := fleetBinary(t, "GPUWORKS_FLEET_HOST_BIN", "./test/l2/cmd/fleet-host")
	workRoot := t.TempDir()
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "password", serverBin, "serve", "--config", loopbackOutboundConfig(t), "--addr", "127.0.0.1:"+port, "--data", workRoot+"/server-data")
	server.Stdout, server.Stderr = fleetProcessLog(t, workRoot, "server.log")
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	client := fleetLogin(t, base)
	token := loginWithPassword(t, base, "password")

	identity, err := client.CreateNode("trace-node")
	if err != nil {
		t.Fatal(err)
	}
	roster, _ := json.Marshal([]fleetRosterEntry{{Name: "trace-node", UUID: identity.UUID, Token: identity.Token}})
	if err := os.WriteFile(workRoot+"/roster.json", roster, 0o644); err != nil {
		t.Fatal(err)
	}
	modelDir := workRoot + "/model"
	host := exec.Command(hostBin, "--roster", workRoot+"/roster.json", "--server", base, "--state-root", workRoot+"/agents",
		"--model-dir", modelDir, "--ready", workRoot+"/ready", "--gpus", "1", "--engine-chunks", "4", "--log", workRoot+"/agent.log")
	host.Env = os.Environ()
	host.Stdout, host.Stderr = fleetProcessLog(t, workRoot, "fleet-host.log")
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(host) })
	waitForFile(t, workRoot+"/ready", time.Minute)
	waitNode(t, base, token, func(n processNode) bool { return n.UUID == identity.UUID && n.State == "online" })

	modelID, err := client.RegisterLocalModel(identity.UUID, modelDir, "qwen3-trace-fake")
	if err != nil {
		t.Fatal(err)
	}
	deploy, _ := json.Marshal(map[string]any{"model_id": modelID, "node_uuid": identity.UUID, "served_name": "trace-model", "quick_params": map[string]any{"max_model_len": 512}})
	var created struct {
		ID uint `json:"id"`
	}
	if err := client.PostJSON("/api/v1/deployments", deploy, &created); err != nil {
		t.Fatal(err)
	}
	waitGatewayInstancesRunning(t, client, 1, 5*time.Minute)
	var key struct {
		Key string `json:"key"`
	}
	if err := client.PostJSON("/api/v1/apikeys", []byte(`{"name":"trace-test"}`), &key); err != nil {
		t.Fatal(err)
	}

	collector := newOTLPCollector(t)
	put := func(body string) {
		t.Helper()
		resp, data := authedDo(t, http.MethodPut, base, token, "/api/v1/telemetry/export", []byte(body))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT telemetry export = %d %s", resp.StatusCode, data)
		}
	}
	put(fmt.Sprintf(`{"enabled":true,"endpoint":%q,"headers":[{"name":"Authorization","value":"Bearer otel-t0k"}],"sample_ratio":1}`, collector.server.URL))

	chat := func(headers map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
			bytes.NewBufferString(`{"model":"trace-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := processHTTPClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "[DONE]") {
			t.Fatalf("chat = %d %s", resp.StatusCode, body)
		}
		return resp, string(body)
	}

	const callerTrace, callerSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	// The server names requests itself (the access log's id); a client's
	// X-Request-ID does not replace it. The caller's trace is what ties the
	// two sides together.
	resp, _ := chat(map[string]string{"traceparent": "00-" + callerTrace + "-" + callerSpan + "-01", "X-Request-ID": "upstream-gw-77"})
	requestID := resp.Header.Get("X-Request-ID")
	if requestID == "" || requestID == "upstream-gw-77" {
		t.Fatalf("X-Request-ID = %q", requestID)
	}
	var span otlpSpanWire
	var auth []string
	deadline := time.Now().Add(30 * time.Second)
	for {
		var ok bool
		if span, auth, ok = collector.span(callerTrace); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no span of the caller's trace reached the collector (%d spans)", collector.count())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if span.ParentSpanID != callerSpan || span.Kind != 2 || span.Name != "chat trace-model" || len(span.SpanID) != 16 || span.SpanID == callerSpan {
		t.Fatalf("span = %+v", span)
	}
	if span.attr("gpuworks.request.id") != requestID || span.attr("gpuworks.deployment.id") != fmt.Sprint(created.ID) ||
		span.attr("http.response.status_code") != "200" || span.attr("gpuworks.request.result") != "completed" || span.attr("gen_ai.usage.output_tokens") == "" {
		t.Fatalf("span attributes = %+v", span.Attributes)
	}
	if len(auth) == 0 || auth[0] != "Bearer otel-t0k" {
		t.Fatalf("collector saw Authorization %q", auth)
	}

	// The request view and the lookup find the request by its trace id.
	var lookup struct {
		Window   string `json:"window"`
		Requests []struct {
			ID           string `json:"id"`
			TraceID      string `json:"trace_id"`
			DeploymentID uint   `json:"deployment_id"`
		} `json:"requests"`
		Deployments []struct {
			ID   uint   `json:"id"`
			Name string `json:"name"`
		} `json:"deployments"`
	}
	deadline = time.Now().Add(30 * time.Second) // ux.db flushes every second
	for {
		if err := client.GetJSON("/api/v1/monitor/requests/lookup?trace_id="+callerTrace, &lookup); err != nil {
			t.Fatal(err)
		}
		if len(lookup.Requests) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(lookup.Requests) != 1 || lookup.Requests[0].ID != requestID || lookup.Requests[0].DeploymentID != created.ID ||
		lookup.Window != "default" || len(lookup.Deployments) != 1 || lookup.Deployments[0].Name != "trace-model" {
		t.Fatalf("lookup = %+v", lookup)
	}
	if err := client.GetJSON("/api/v1/monitor/requests/lookup?request_id="+requestID, &lookup); err != nil || len(lookup.Requests) != 1 || lookup.Requests[0].TraceID != callerTrace {
		t.Fatalf("lookup by request id = %+v, %v", lookup, err)
	}
	var history struct {
		Requests []struct {
			ID      string `json:"id"`
			TraceID string `json:"trace_id"`
		} `json:"requests"`
	}
	if err := client.GetJSON(fmt.Sprintf("/api/v1/monitor/deployments/%d/requests?view=history&range=1h", created.ID), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Requests) == 0 || history.Requests[0].TraceID != callerTrace {
		t.Fatalf("history = %+v", history)
	}

	gatewayTokenizeIsForwardedButNotTraffic(t, base, token, key.Key, client, created.ID, collector, len(history.Requests))

	// A request without a trace starts one; its id carries its time, so the
	// lookup needs no window.
	before := collector.count()
	chat(nil)
	deadline = time.Now().Add(30 * time.Second)
	for collector.count() == before {
		if time.Now().After(deadline) {
			t.Fatal("no span for a request without a trace")
		}
		time.Sleep(200 * time.Millisecond)
	}
	collector.mu.Lock()
	root := collector.spans[len(collector.spans)-1]
	collector.mu.Unlock()
	if root.ParentSpanID != "" || len(root.TraceID) != 32 {
		t.Fatalf("root span = %+v", root)
	}
	deadline = time.Now().Add(30 * time.Second)
	for {
		if err := client.GetJSON("/api/v1/monitor/requests/lookup?trace_id="+root.TraceID, &lookup); err != nil {
			t.Fatal(err)
		}
		if len(lookup.Requests) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(lookup.Requests) != 1 || lookup.Window != "trace_time" {
		t.Fatalf("lookup of a gateway-started trace = %+v", lookup)
	}

	// Off: no more spans; the status says so.
	put(fmt.Sprintf(`{"enabled":false,"endpoint":%q,"headers":[{"name":"Authorization"}],"sample_ratio":1}`, collector.server.URL))
	count := collector.count()
	chat(map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	time.Sleep(3 * time.Second)
	if collector.count() != count {
		t.Fatalf("spans kept arriving after export was turned off: %d -> %d", count, collector.count())
	}
	var status struct {
		Enabled bool `json:"enabled"`
		Headers []struct {
			Name string `json:"name"`
		} `json:"headers"`
		Status struct {
			Active   bool   `json:"active"`
			Exported uint64 `json:"exported"`
		} `json:"status"`
	}
	if err := client.GetJSON("/api/v1/telemetry/export", &status); err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.Status.Active || status.Status.Exported < 2 || len(status.Headers) != 1 || status.Headers[0].Name != "Authorization" {
		t.Fatalf("status = %+v", status)
	}
}
