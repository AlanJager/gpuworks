package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

// realSample is a PD exchange recorded against real vLLM (phase 0).
type realSample struct {
	PrefillResponse map[string]json.RawMessage `json:"prefill_response"`
	DecodeStream    string                     `json:"decode_stream"`
}

func loadSample(t *testing.T, version string) realSample {
	t.Helper()
	raw, err := os.ReadFile("testdata/pd/vllm-" + version + "-nixl.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample realSample
	if err := json.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	return sample
}

func keysOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range doc {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type pdPair struct {
	prefill, decode *httptest.Server
	decodeEngine    *Engine
	crashes         []string
}

func newPDPair(t *testing.T, policy string, disabled bool) *pdPair {
	t.Helper()
	pair := &pdPair{}
	side := freePort(t)
	producer := New(Options{Model: "qwen", PromptTokens: 100, PD: &PDOptions{Role: "kv_producer", EngineID: "eng-p", SideHost: "127.0.0.1", SidePort: side, Disabled: disabled}})
	stop, err := producer.ServeSideChannel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	pair.prefill = httptest.NewServer(producer)
	t.Cleanup(pair.prefill.Close)
	pair.decodeEngine = New(Options{Model: "qwen", PromptTokens: 100, PD: &PDOptions{Role: "kv_consumer", SideHost: "127.0.0.1", SidePort: freePort(t), FailurePolicy: policy, Crash: func(reason string) { pair.crashes = append(pair.crashes, reason) }}})
	pair.decode = httptest.NewServer(pair.decodeEngine)
	t.Cleanup(pair.decode.Close)
	return pair
}

func pdPost(t *testing.T, url, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]json.RawMessage
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func metricValue(t *testing.T, url, series string) string {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, series+" ") {
			return strings.TrimPrefix(line, series+" ")
		}
	}
	return ""
}

func TestPDProducerMatchesRealResponseShape(t *testing.T) {
	sample := loadSample(t, "0.25.1")
	pair := newPDPair(t, "", false)
	status, answer := pdPost(t, pair.prefill.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"kv_transfer_params":{"do_remote_decode":true}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got, want := keysOf(t, answer["kv_transfer_params"]), keysOf(t, sample.PrefillResponse["kv_transfer_params"]); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("kv_transfer_params keys = %v, real vLLM has %v", got, want)
	}
	for _, key := range []string{"id", "object", "choices", "usage", "kv_transfer_params"} {
		if _, ok := sample.PrefillResponse[key]; !ok {
			t.Fatalf("real sample lacks %s; the fixture contract is stale", key)
		}
		if _, ok := answer[key]; !ok {
			t.Fatalf("fixture lacks %s", key)
		}
	}
	// 0.17.1 hands out the same keys minus remote_num_tokens, and a flat
	// block list: the gateway must not depend on either.
	old := loadSample(t, "0.17.1")
	if strings.Contains(string(old.PrefillResponse["kv_transfer_params"]), "[[") {
		t.Fatal("0.17.1 sample was expected to carry a flat block list")
	}
}

func TestPDConsumerPullsOnceAndCountsTheTransfer(t *testing.T) {
	pair := newPDPair(t, "", false)
	_, answer := pdPost(t, pair.prefill.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":{"do_remote_decode":true}}`)
	params := string(answer["kv_transfer_params"])
	status, _ := pdPost(t, pair.decode.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":`+params+`}`)
	if status != http.StatusOK {
		t.Fatalf("decode status = %d", status)
	}
	if v := metricValue(t, pair.decode.URL, `vllm:prompt_tokens_by_source_total{engine="0",model_name="qwen",source="external_kv_transfer"}`); v != "100" {
		t.Fatalf("external tokens = %q", v)
	}
	if v := metricValue(t, pair.decode.URL, `vllm:nixl_bytes_transferred_count{engine="0",model_name="qwen"}`); v != "1" {
		t.Fatalf("transfers = %q", v)
	}
	// NIXL is pulled by the decoder: the producer's transfer counters stay 0.
	if v := metricValue(t, pair.prefill.URL, `vllm:nixl_bytes_transferred_count{engine="0",model_name="qwen"}`); v != "0" {
		t.Fatalf("producer transfers = %q", v)
	}
	// The same params again: the KV was already pulled, so the fail policy
	// answers 500 and counts a failed transfer; the engine stays up.
	status, body := pdPost(t, pair.decode.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":`+params+`}`)
	if status != http.StatusInternalServerError || !strings.Contains(string(body["error"]), "Internal server error") {
		t.Fatalf("second pull = %d %v", status, body)
	}
	if v := metricValue(t, pair.decode.URL, `vllm:nixl_num_failed_transfers_total{engine="0",model_name="qwen"}`); v != "1" {
		t.Fatalf("failed = %q", v)
	}
	// Without params the decoder computes locally.
	if status, _ := pdPost(t, pair.decode.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusOK {
		t.Fatalf("local = %d", status)
	}
	if v := metricValue(t, pair.decode.URL, `vllm:prompt_tokens_by_source_total{engine="0",model_name="qwen",source="local_compute"}`); v != "100" {
		t.Fatalf("local tokens = %q", v)
	}
}

func TestPDConsumerRecomputePolicy(t *testing.T) {
	pair := newPDPair(t, "recompute", false)
	bogus := fmt.Sprintf(`{"do_remote_prefill":true,"remote_request_id":"gone","remote_engine_id":"x","remote_host":"127.0.0.1","remote_port":%d,"remote_block_ids":[[1]]}`, freePort(t))
	if status, _ := pdPost(t, pair.decode.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":`+bogus+`}`); status != http.StatusOK {
		t.Fatalf("recompute status = %d", status)
	}
	if v := metricValue(t, pair.decode.URL, `vllm:nixl_num_failed_transfers_total{engine="0",model_name="qwen"}`); v != "1" {
		t.Fatalf("failed = %q", v)
	}
}

func TestPDConsumerCrashesOnMalformedParams(t *testing.T) {
	pair := newPDPair(t, "", false)
	status, _ := pdPost(t, pair.decode.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":{"do_remote_prefill":true,"remote_engine_id":"x","remote_block_ids":[1],"remote_host":"127.0.0.1","remote_port":1}}`)
	if status != http.StatusInternalServerError || len(pair.crashes) != 1 || !strings.Contains(pair.crashes[0], "remote_request_id") {
		t.Fatalf("status %d crashes %v", status, pair.crashes)
	}
}

func TestPDDisabledProducerAnswersWithoutParams(t *testing.T) {
	pair := newPDPair(t, "", true)
	_, answer := pdPost(t, pair.prefill.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"kv_transfer_params":{"do_remote_decode":true}}`)
	if string(answer["kv_transfer_params"]) != "null" {
		t.Fatalf("params = %s", answer["kv_transfer_params"])
	}
}

func TestPDFromLaunch(t *testing.T) {
	env := map[string]string{"VLLM_NIXL_SIDE_CHANNEL_HOST": "10.0.0.5", "VLLM_NIXL_SIDE_CHANNEL_PORT": "30001"}
	pd := PDFromLaunch([]string{"--kv-transfer-config", `{"kv_connector":"MultiConnector","kv_role":"kv_producer","kv_load_failure_policy":"recompute","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector","kv_role":"kv_producer"},{"kv_connector":"OffloadingConnector","kv_role":"kv_both"}]}}`}, env)
	if pd == nil || pd.Role != "kv_producer" || pd.SideHost != "10.0.0.5" || pd.SidePort != 30001 || pd.FailurePolicy != "recompute" || pd.Disabled {
		t.Fatalf("pd = %+v", pd)
	}
	if PDFromLaunch([]string{"--kv-transfer-config", `{"kv_connector":"OffloadingConnector","kv_role":"kv_both"}`}, nil) != nil {
		t.Fatal("offload alone is not PD")
	}
	replaced := PDFromLaunch([]string{"--kv-offloading-size", "2", "--kv-transfer-config", `{"kv_connector":"NixlConnector","kv_role":"kv_consumer"}`}, nil)
	if replaced == nil || !replaced.Disabled || replaced.SideHost != "localhost" || replaced.SidePort != 5600 {
		t.Fatalf("replaced = %+v", replaced)
	}
}

func TestPDConcurrentDialect(t *testing.T) {
	side := freePort(t)
	producer := New(Options{Model: "qwen", PromptTokens: 40, PD: PDFromLaunch([]string{"--disaggregation-mode", "prefill", "--disaggregation-bootstrap-port", fmt.Sprint(side), "--host", "127.0.0.1"}, nil)})
	stop, err := producer.ServeSideChannel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	prefill := httptest.NewServer(producer)
	t.Cleanup(prefill.Close)
	decode := httptest.NewServer(New(Options{Model: "qwen", PromptTokens: 40, PD: PDFromLaunch([]string{"--disaggregation-mode", "decode"}, nil)}))
	t.Cleanup(decode.Close)
	body := fmt.Sprintf(`{"model":"qwen","messages":[{"role":"user","content":"hi"}],"bootstrap_host":"127.0.0.1","bootstrap_port":%d,"bootstrap_room":42}`, side)
	// The decoder may arrive first: it waits for the room.
	done := make(chan int, 1)
	go func() {
		status, _ := pdPost(t, decode.URL+"/v1/chat/completions", body)
		done <- status
	}()
	if status, _ := pdPost(t, prefill.URL+"/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("prefill = %d", status)
	}
	if status := <-done; status != http.StatusOK {
		t.Fatalf("decode = %d", status)
	}
	if v := metricValue(t, decode.URL, `vllm:prompt_tokens_by_source_total{engine="0",model_name="qwen",source="external_kv_transfer"}`); v != "40" {
		t.Fatalf("external = %q", v)
	}
	if status, _ := pdPost(t, decode.URL+"/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`); status != http.StatusBadRequest {
		t.Fatalf("decode without bootstrap = %d", status)
	}
}
