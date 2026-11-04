package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBootstrap answers /query like vLLM's MooncakeBootstrapServer: per data
// parallel rank (a JSON object key, as FastAPI writes an int key), the engine
// id and every registered (TP rank, PP rank) worker's ZMQ address. mode
// switches in the failures.
type fakeBootstrap struct {
	mu       sync.Mutex
	engineID string
	workers  int    // registered TP ranks
	mode     string // "", "500", "garbage", "empty"
	queries  int
}

func (f *fakeBootstrap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if r.URL.Path != "/query" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	switch f.mode {
	case "500":
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	case "garbage":
		_, _ = w.Write([]byte("<html>"))
		return
	case "empty":
		_, _ = w.Write([]byte("{}"))
		return
	}
	workers := map[string]map[string]string{}
	for i := 0; i < f.workers; i++ {
		workers[fmt.Sprint(i)] = map[string]string{"0": fmt.Sprintf("tcp://10.0.0.9:%d", 41000+i)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"0": map[string]any{"engine_id": f.engineID, "worker_addr": workers}})
}

// newBootstrapHarness is a deployment of a synthesized-handoff mode
// (vllm-mooncake, vllm-ppu-mooncake): every prefill instance answers no
// kv_transfer_params, as vLLM's MooncakeConnector producer does, and serves a
// bootstrap server on its KV port (API port + 100 in the harness's launch
// records).
func newBootstrapHarness(t *testing.T, prefills int, fallback string) (*pdHarness, []*fakeBootstrap) {
	t.Helper()
	return newBootstrapHarnessMode(t, "vllm-mooncake", prefills, fallback)
}

func newBootstrapHarnessMode(t *testing.T, mode string, prefills int, fallback string) (*pdHarness, []*fakeBootstrap) {
	t.Helper()
	h := newPDHarnessMode(t, mode, prefills, fallback)
	var boots []*fakeBootstrap
	for i, f := range h.prefills {
		f.mode = "no_params"
		boot := &fakeBootstrap{engineID: fmt.Sprintf("mooncake-engine-%d", i), workers: 1}
		server := httptest.NewServer(boot)
		t.Cleanup(server.Close)
		h.hub.mu.Lock()
		h.hub.routes[fmt.Sprintf("127.0.0.1:%d", 32100+i)] = strings.TrimPrefix(server.URL, "http://")
		h.hub.mu.Unlock()
		boots = append(boots, boot)
	}
	return h, boots
}

func TestPDBootstrapWireNamesTheHandoffOnBothInstances(t *testing.T) {
	h, boots := newBootstrapHarness(t, 1, "")
	for i := 0; i < 2; i++ {
		rec := h.do(t, "/v1/chat/completions", strings.Replace(pdChat, "Rome", fmt.Sprintf("Rome %d", i), 1))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
			t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(h.prefills[0].bodies) != 2 || len(h.decode.bodies) != 2 {
		t.Fatalf("calls: prefill %d decode %d", len(h.prefills[0].bodies), len(h.decode.bodies))
	}
	seen := map[string]bool{}
	for i := range h.decode.bodies {
		pkv, _ := h.prefills[0].bodies[i]["kv_transfer_params"].(map[string]any)
		tid, _ := pkv["transfer_id"].(string)
		if len(pkv) != 2 || pkv["do_remote_decode"] != true || !strings.HasPrefix(tid, "gw-") {
			t.Fatalf("prefill kv params = %v", pkv)
		}
		if seen[tid] {
			t.Fatalf("transfer id %s reused", tid)
		}
		seen[tid] = true
		dkv, _ := h.decode.bodies[i]["kv_transfer_params"].(map[string]any)
		want := map[string]any{"do_remote_prefill": true, "remote_engine_id": "mooncake-engine-0", "remote_bootstrap_addr": "http://10.0.0.9:32100", "transfer_id": tid}
		if fmt.Sprint(dkv) != fmt.Sprint(want) {
			t.Fatalf("decode kv params = %v, want %v", dkv, want)
		}
		if h.decode.bodies[i]["stream"] != true || h.decode.bodies[i]["max_tokens"] != float64(64) {
			t.Fatalf("decode body = %v", h.decode.bodies[i])
		}
	}
	// The engine id is asked for once per prefill instance.
	if boots[0].queries != 1 {
		t.Fatalf("bootstrap queries = %d", boots[0].queries)
	}
	if got := h.acct.PDFallbacks(h.dep.ID); len(got) != 0 {
		t.Fatalf("fallbacks = %v", got)
	}
}

func TestPDBootstrapWireSkipsAPrefillItCannotPointTheDecoderAt(t *testing.T) {
	for _, mode := range []string{"500", "garbage", "empty", "unreachable", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			h, boots := newBootstrapHarness(t, 2, "")
			switch mode {
			case "unreachable":
				h.hub.mu.Lock()
				delete(h.hub.routes, "127.0.0.1:32100")
				h.hub.mu.Unlock()
			case "incomplete":
				boots[0].workers = 0
			default:
				boots[0].mode = mode
			}
			// Without an affinity the less loaded instance is first; make
			// the broken one the only choice of the first attempt by asking
			// twice and checking every handoff went to the healthy one.
			for i := 0; i < 3; i++ {
				if rec := h.do(t, "/v1/chat/completions", strings.Replace(pdChat, "Rome", fmt.Sprintf("Rome %d", i), 1)); rec.Code != http.StatusOK {
					t.Fatalf("response = %d", rec.Code)
				}
			}
			if len(h.prefills[0].bodies) != 0 {
				t.Fatalf("the prefill instance without a usable bootstrap got %d copies", len(h.prefills[0].bodies))
			}
			for _, body := range h.decode.bodies {
				kv, _ := body["kv_transfer_params"].(map[string]any)
				if kv["remote_engine_id"] != "mooncake-engine-1" {
					t.Fatalf("decode kv = %v", body["kv_transfer_params"])
				}
			}
			if got := h.acct.PDFallbacks(h.dep.ID); len(got) != 0 {
				t.Fatalf("fallbacks = %v", got)
			}
		})
	}
}

func TestPDBootstrapWireFallsBackWhenNoBootstrapIsUsable(t *testing.T) {
	h, boots := newBootstrapHarness(t, 1, "")
	boots[0].workers = 0
	if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusOK {
		t.Fatalf("response = %d", rec.Code)
	}
	if len(h.prefills[0].bodies) != 0 {
		t.Fatal("a prefill copy went out without a bootstrap the decoder could use")
	}
	if kv, present := h.decode.bodies[0]["kv_transfer_params"]; present && kv != nil {
		t.Fatalf("fallback request carried kv params: %v", kv)
	}
	if got := h.acct.PDFallbacks(h.dep.ID); got[pdFallbackNoBootstrap] != 1 {
		t.Fatalf("fallbacks = %v", got)
	}
	// An incomplete answer is not cached: once every rank registered, the
	// next request hands off.
	boots[0].mu.Lock()
	boots[0].workers = 1
	boots[0].mu.Unlock()
	if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusOK {
		t.Fatalf("response = %d", rec.Code)
	}
	if kv, _ := h.decode.bodies[1]["kv_transfer_params"].(map[string]any); kv["remote_engine_id"] != "mooncake-engine-0" {
		t.Fatalf("decode kv = %v", h.decode.bodies[1]["kv_transfer_params"])
	}
}

func TestPDBootstrapWireStrictFallbackAnswers503(t *testing.T) {
	h, boots := newBootstrapHarness(t, 1, "fail")
	boots[0].mode = "500"
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), pdFallbackNoBootstrap) {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(h.decode.bodies) != 0 {
		t.Fatal("a strict deployment sent the request to its decoder")
	}
}

func TestPDBootstrapEngineIDWaitsForEveryTPRank(t *testing.T) {
	h, boots := newBootstrapHarness(t, 1, "")
	c := candidate{instanceUUID: "prefill-0", nodeUUID: "node-pd", kvHost: "10.0.0.9", kvPort: 32100}
	boots[0].workers = 1
	if _, err := h.proxy.bootstrapEngineID(context.Background(), c, 2); err != errBootstrapIncomplete {
		t.Fatalf("one of two ranks: err = %v", err)
	}
	boots[0].workers = 2
	id, err := h.proxy.bootstrapEngineID(context.Background(), c, 2)
	if err != nil || id != "mooncake-engine-0" {
		t.Fatalf("id = %q err = %v", id, err)
	}
	if _, err := h.proxy.bootstrapEngineID(context.Background(), candidate{instanceUUID: "x"}, 1); err == nil {
		t.Fatal("an instance without a bootstrap port was asked")
	}
}

func TestBootstrapEnginesExpire(t *testing.T) {
	var b bootstrapEngines
	now := time.Unix(1000, 0)
	b.put("a", "engine-a", now)
	if id, ok := b.get("a", now.Add(time.Minute)); !ok || id != "engine-a" {
		t.Fatalf("get = %q %v", id, ok)
	}
	if _, ok := b.get("a", now.Add(bootstrapEngineTTL+time.Second)); ok {
		t.Fatal("an expired entry was served")
	}
	b.put("b", "engine-b", now.Add(bootstrapEngineTTL+time.Second))
	if _, ok := b.entries["a"]; ok {
		t.Fatal("an expired entry was kept")
	}
}

func TestVerifyPDHandoffOverTheBootstrapWire(t *testing.T) {
	h, boots := newBootstrapHarness(t, 1, "")
	out, err := h.proxy.VerifyPDHandoff(context.Background(), h.dep.ID, "prefill-0")
	if err != nil || out.Failed || out.Measured {
		t.Fatalf("handoff = %+v err = %v", out, err)
	}
	pkv, _ := h.prefills[0].bodies[0]["kv_transfer_params"].(map[string]any)
	var dkv map[string]any
	for i, path := range h.decode.paths {
		// The decoder's metrics are read around the handoff.
		if path == "/v1/completions" {
			dkv, _ = h.decode.bodies[i]["kv_transfer_params"].(map[string]any)
		}
	}
	if pkv["transfer_id"] == nil || pkv["transfer_id"] != dkv["transfer_id"] || dkv["remote_engine_id"] != "mooncake-engine-0" {
		t.Fatalf("prefill kv %v decode kv %v", pkv, dkv)
	}
	boots[0].mode = "500"
	h.proxy.bootstrap = bootstrapEngines{}
	out, err = h.proxy.VerifyPDHandoff(context.Background(), h.dep.ID, "prefill-0")
	if err != nil || !out.Failed || out.Reason != pdFallbackNoBootstrap {
		t.Fatalf("handoff = %+v err = %v", out, err)
	}
}
