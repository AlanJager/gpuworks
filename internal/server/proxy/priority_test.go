package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

type priorityRig struct {
	p  *Proxy
	st *store.Store
	mu sync.Mutex
	// seen is what the engine got: its priority field (absent: nil) and
	// whether vLLM's header reached it.
	seen []map[string]any
	hdr  []string
}

// newPriorityRig serves three deployments on one engine stub: "v" (vLLM with
// priority scheduling), "s" (SGLang with it) and "off" (vLLM without).
func newPriorityRig(t *testing.T) *priorityRig {
	t.Helper()
	rig := &priorityRig{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rig.mu.Lock()
		rig.seen = append(rig.seen, body)
		rig.hdr = append(rig.hdr, r.Header.Get("X-Vllm-Priority"))
		rig.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_priority_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-prio", store.HashToken("node"), "node-prio")
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range []struct {
		name, engine string
		on           bool
	}{{"v", "vllm", true}, {"s", "sglang", true}, {"off", "vllm", false}} {
		dep, err := st.CreateDeployment(&store.Deployment{Name: d.name, ModelID: model.ID, NodeID: node.ID, ServedName: d.name, Engine: d.engine, State: "active", PriorityScheduling: d.on})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: d.name + "-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000 + i, Ports: "[30000]"}); err != nil {
			t.Fatal(err)
		}
	}
	addr := strings.TrimPrefix(upstream.URL, "http://")
	rig.p = New(st, storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(rig.p.Close)
	rig.st = st
	return rig
}

func (r *priorityRig) key(t *testing.T, binding store.APIKeyPriority) string {
	t.Helper()
	full, err := r.st.CreateAPIKey("k-"+string(binding.Policy)+binding.Class+binding.Ceiling, 0)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := r.st.ResolveAPIKey(full)
	if !ok {
		t.Fatal("key does not resolve")
	}
	keys, err := r.st.ListAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.ID == identity.KeyID {
			if err := r.st.SetAPIKeyPriority(k.Prefix, binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	return full
}

// send posts a chat to model and returns the answer and what the engine got.
func (r *priorityRig) send(t *testing.T, key, model, asked, body string) (*httptest.ResponseRecorder, map[string]any, string) {
	t.Helper()
	if body == "" {
		body = `{"model":"` + model + `","messages":[]}`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Vllm-Priority", "-1000")
	if asked != "" {
		req.Header.Set(priorityClassHeader, asked)
	}
	rec := httptest.NewRecorder()
	r.p.Handler()(rec, req)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return rec, nil, ""
	}
	got, hdr := r.seen[len(r.seen)-1], r.hdr[len(r.hdr)-1]
	r.seen, r.hdr = nil, nil
	return rec, got, hdr
}

func TestGatewayWritesTheEnginePriority(t *testing.T) {
	rig := newPriorityRig(t)
	high := rig.key(t, store.APIKeyPriority{Class: priority.High})
	unbound := rig.key(t, store.APIKeyPriority{})
	ceiling := rig.key(t, store.APIKeyPriority{Class: priority.Normal, Ceiling: priority.Critical})
	fixed := rig.key(t, store.APIKeyPriority{Class: priority.Low, Ceiling: priority.Critical, Policy: priority.PolicyFixed})

	for _, tc := range []struct {
		name, key, model, asked, body string
		class                         string
		engine                        any // the engine's priority field; nil: absent
	}{
		{name: "vllm, key's class, client value replaced", key: high, model: "v", body: `{"model":"v","priority":99,"messages":[]}`, class: "high", engine: float64(-20)},
		{name: "sglang runs higher-first", key: high, model: "s", class: "high", engine: float64(20)},
		{name: "unbound key runs at the default", key: unbound, model: "v", class: "normal", engine: nil},
		{name: "a lower ask", key: high, model: "v", asked: "batch", class: "batch", engine: float64(40)},
		{name: "above the key's own class is lowered", key: high, model: "s", asked: "critical", class: "high", engine: float64(20)},
		{name: "up to the ceiling", key: ceiling, model: "s", asked: "critical", class: "critical", engine: float64(40)},
		{name: "fixed ignores the ask", key: fixed, model: "s", asked: "critical", class: "low", engine: float64(-20)},
		{name: "no priority scheduling: neutral, client value replaced", key: high, model: "off", body: `{"model":"off","priority":99,"messages":[]}`, class: "high", engine: float64(0)},
		{name: "no priority scheduling, no field: nothing written", key: high, model: "off", class: "high", engine: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, seen, hdr := rig.send(t, tc.key, tc.model, tc.asked, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get(priorityClassHeader); got != tc.class {
				t.Fatalf("answered class = %q, want %q", got, tc.class)
			}
			if hdr != "" {
				t.Fatalf("X-Vllm-Priority reached the engine: %q", hdr)
			}
			if got, present := seen["priority"]; (tc.engine == nil && present) || (tc.engine != nil && got != tc.engine) {
				t.Fatalf("engine priority = %v (present %v), want %v", got, present, tc.engine)
			}
			if _, leaked := seen[strings.ToLower(priorityClassHeader)]; leaked {
				t.Fatal("the class header leaked into the body")
			}
		})
	}

	// An unknown class is refused before any engine sees the request.
	rec, seen, _ := rig.send(t, high, "v", "platinum", "")
	if rec.Code != http.StatusBadRequest || seen != nil || !strings.Contains(rec.Body.String(), "priority class") {
		t.Fatalf("unknown class = %d %s seen=%v", rec.Code, rec.Body, seen)
	}
}

// A class change on this server applies to the next request, with no wait
// for the snapshot to age.
func TestGatewayPriorityFollowsClassChanges(t *testing.T) {
	rig := newPriorityRig(t)
	if err := rig.st.CreatePriorityClass(&store.PriorityClass{Name: "gold", Value: 30}); err != nil {
		t.Fatal(err)
	}
	key := rig.key(t, store.APIKeyPriority{Class: "gold"})
	if _, seen, _ := rig.send(t, key, "s", "", ""); seen["priority"] != float64(30) {
		t.Fatalf("gold = %v", seen["priority"])
	}
	v := 35
	if _, err := rig.st.UpdatePriorityClass("gold", store.PriorityClassUpdate{Value: &v}); err != nil {
		t.Fatal(err)
	}
	if _, seen, _ := rig.send(t, key, "s", "", ""); seen["priority"] != float64(35) {
		t.Fatalf("after the edit = %v", seen["priority"])
	}
	// The default moves for unbound keys too.
	unbound := rig.key(t, store.APIKeyPriority{})
	if err := rig.st.SetDefaultPriorityClass(priority.Low); err != nil {
		t.Fatal(err)
	}
	if rec, seen, _ := rig.send(t, unbound, "s", "", ""); seen["priority"] != float64(-20) || rec.Header().Get(priorityClassHeader) != "low" {
		t.Fatalf("new default = %v %q", seen["priority"], rec.Header().Get(priorityClassHeader))
	}
}

// Batch requests always run at the batch class, whatever their key.
func TestBatchRequestsRunAtTheBatchClass(t *testing.T) {
	rig := newPriorityRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.Critical})
	identity, _ := rig.st.ResolveAPIKey(key)
	reply := rig.p.Do(t.Context(), uxrecord.Caller{APIKeyID: identity.KeyID, Source: uxrecord.SourceBatch}, "/v1/chat/completions", []byte(`{"model":"s","messages":[]}`))
	if reply.Err != nil || reply.Status != http.StatusOK {
		t.Fatalf("batch = %+v", reply)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.seen) != 1 || rig.seen[0]["priority"] != float64(-40) {
		t.Fatalf("batch priority = %v", rig.seen)
	}
}

// The snapshot of another server's changes is no older than its bound.
func TestPriorityClassSnapshotAges(t *testing.T) {
	rig := newPriorityRig(t)
	clock := time.Unix(1000, 0)
	cache := &priorityClasses{store: rig.st, now: func() time.Time { return clock }}
	if cache.get().Default.Name != priority.Normal {
		t.Fatal("first load")
	}
	// A change the generation does not see (another server's) is picked up
	// once the snapshot ages.
	if err := rig.st.SetDefaultPriorityClass(priority.High); err != nil {
		t.Fatal(err)
	}
	// Pretend the change came from another server: this process's
	// generation does not move it.
	stale := cache.snap.Load()
	cache.snap.Store(&prioritySnapshot{table: stale.table, gen: rig.st.PriorityClassGeneration(), at: stale.at})
	if cache.get().Default.Name != priority.Normal {
		t.Fatal("a fresh snapshot reloaded without cause")
	}
	clock = clock.Add(priorityClassMaxAge)
	if cache.get().Default.Name != priority.High {
		t.Fatal("an aged snapshot did not reload")
	}
}
