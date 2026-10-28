package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// fakeAPI is an APIReader over a real chi router, as the api package's
// aiReader is: Route matches concrete paths to their patterns the way the
// router does, Get refuses a route Readable denies (or that no route
// matches) without serving it, and handlers answer the wire shapes the real
// endpoints answer (docs/reference/api.md): collections as {"items",
// "total"}, product errors as {"code","error","params"} with a 4xx status.
// Every served read is recorded as path?query.
type fakeAPI struct {
	mux   *chi.Mux
	gone  map[string]bool
	nodes []fakeNode
	deps  []map[string]any

	mu    sync.Mutex
	calls []string
}

type fakeNode struct {
	UUID, Name, Hostname, State, Token string
	GPUs                               []string
	LastSeen                           time.Time
	Created                            time.Time
}

// The fixture clock: node and log times are written relative to it.
var fakeNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

const (
	uuidA = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"
	uuidB = "f9e8d7c6b5a4938271605f4e3d2c1b0a"
)

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{mux: chi.NewRouter()}
	f.nodes = []fakeNode{
		{UUID: uuidA, Name: "gpu-a", Hostname: "host-a", State: "online", Token: "nt_secret_a", GPUs: []string{"H100", "H100"},
			LastSeen: fakeNow.Add(-30 * time.Second), Created: fakeNow.Add(-10 * 24 * time.Hour)},
		{UUID: uuidB, Name: "gpu-b", Hostname: "host-b", State: "offline", Token: "nt_secret_b", GPUs: []string{"A100"},
			LastSeen: fakeNow.Add(-90 * time.Minute), Created: fakeNow.Add(-4 * 24 * time.Hour)},
	}
	f.deps = []map[string]any{
		{"id": 12, "name": "qwen", "model_name": "Qwen3-8B", "state": "running", "runtime_state": "running", "replicas": 1, "ready_replicas": 1, "node_names": []string{"gpu-a"}, "created_at": fakeNow.Add(-2 * time.Hour).Format(time.RFC3339)},
		{"id": 13, "name": "llama", "model_name": "Llama-3-70B", "state": "failed", "runtime_state": "failed", "replicas": 2, "ready_replicas": 0, "node_names": []string{"gpu-b"}, "created_at": fakeNow.Add(-3 * time.Hour).Format(time.RFC3339)},
	}
	r := f.mux
	r.Get("/api/v1/nodes", f.listNodes)
	r.Get("/api/v1/nodes/facets", func(w http.ResponseWriter, _ *http.Request) { writeFake(w, 200, map[string]any{"total": len(f.nodes)}) })
	r.Get("/api/v1/nodes/{uuid}", f.nodeDetail)
	r.Get("/api/v1/nodes/{uuid}/ssh", func(w http.ResponseWriter, _ *http.Request) {
		writeFake(w, 200, map[string]any{"user": "root", "password": "hunter2"})
	})
	r.Get("/api/v1/nodes/{uuid}/terminal", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(101) })
	r.Get("/api/v1/apikeys", func(w http.ResponseWriter, _ *http.Request) {
		writeFake(w, 200, map[string]any{"items": []any{}, "total": 0})
	})
	r.Get("/api/v1/events", func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", "text/event-stream") })
	r.Get("/api/v1/undocumented/thing", func(w http.ResponseWriter, _ *http.Request) { writeFake(w, 200, map[string]any{}) })
	r.Get("/api/v1/deployments", f.listDeployments)
	r.Get("/api/v1/deployments/{id}", func(w http.ResponseWriter, req *http.Request) {
		for _, d := range f.deps {
			if fmt.Sprint(d["id"]) == chi.URLParam(req, "id") {
				writeFake(w, 200, d)
				return
			}
		}
		writeProductErr(w, 404, "DEPLOYMENT_NOT_FOUND", "deployment not found", nil)
	})
	r.Get("/api/v1/dashboard/summary", func(w http.ResponseWriter, _ *http.Request) {
		writeFake(w, 200, map[string]any{
			"nodes":       map[string]any{"total": 2, "online": 1, "offline": 1, "gpus": 3},
			"deployments": map[string]any{"total": 2, "running": 1, "failed": 1},
			"by_vendor":   []any{map[string]any{"vendor": "nvidia", "gpus": 3}},
			"by_label":    []any{},
		})
	})
	r.Get("/api/v1/gpus", func(w http.ResponseWriter, req *http.Request) {
		writeFake(w, 200, map[string]any{
			"items": []any{map[string]any{"uuid": "GPU-1", "node": uuidA, "name": "H100"}}, "page": 1, "page_size": 1, "total": 3,
			"overview": map[string]any{"total": 3, "healthy": 2, "unhealthy": 1, "allocated": 1},
			"facets":   map[string]any{"nodes": []any{}, "models": []any{}, "vendors": []any{}},
		})
	})
	r.Get("/api/v1/logs/search", f.searchLogs)
	r.Get("/api/v1/monitor/metrics", func(w http.ResponseWriter, _ *http.Request) { writeFake(w, 200, fakeMetricCatalog()) })
	r.Get("/api/v1/benchmarks/{id}/report", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte("# Benchmark report\n" + strings.Repeat("row\n", 4000)))
	})
	return f
}

// handle replaces or adds a GET handler.
func (f *fakeAPI) handle(pattern string, h http.HandlerFunc) { f.mux.Get(pattern, h) }

func (f *fakeAPI) Routes() []string {
	var out []string
	_ = chi.Walk(f.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && strings.HasPrefix(route, "/api/v1/") {
			out = append(out, route)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (f *fakeAPI) Route(path string) string {
	pattern := f.mux.Find(chi.NewRouteContext(), http.MethodGet, path)
	if f.gone[pattern] {
		return ""
	}
	return pattern
}

// remove makes a route unknown, as on a server that lacks it.
func (f *fakeAPI) remove(pattern string) {
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[pattern] = true
}

func (f *fakeAPI) Get(ctx context.Context, path string, query url.Values) (int, []byte, error) {
	pattern := f.Route(path)
	if pattern == "" || !Readable(pattern) {
		return 0, nil, errors.New("not a readable API route: " + path)
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	f.mu.Lock()
	f.calls = append(f.calls, target)
	f.mu.Unlock()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes(), nil
}

func (f *fakeAPI) issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func writeFake(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProductErr(w http.ResponseWriter, code int, errCode, msg string, params map[string]any) {
	body := map[string]any{"error": msg, "code": errCode}
	if len(params) > 0 {
		body["params"] = params
	}
	writeFake(w, code, body)
}

// listPage parses page/page_size and state like the api's list query: a page
// is asked for by either parameter, page_size is 1-200, and an unknown state
// is a LIST_FILTER_INVALID.
func listPage(w http.ResponseWriter, req *http.Request, states ...string) (paged bool, page, size int, want map[string]bool, ok bool) {
	q := req.URL.Query()
	page, size = 1, 50
	for _, p := range []struct {
		name string
		dst  *int
		max  int
	}{{"page", &page, 1 << 30}, {"page_size", &size, 200}} {
		raw, set := q[p.name]
		if !set {
			continue
		}
		paged = true
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > p.max {
			writeProductErr(w, 400, "LIST_PAGE_INVALID", fmt.Sprintf("%s must be a whole number (page from 1, page_size from 1 to 200), got %q", p.name, raw[0]),
				map[string]any{"name": p.name, "value": raw[0], "max_page_size": 200})
			return false, 0, 0, nil, false
		}
		*p.dst = n
	}
	if raw := q.Get("state"); raw != "" {
		want = map[string]bool{}
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			known := false
			for _, k := range states {
				known = known || k == s
			}
			if !known {
				writeProductErr(w, 400, "LIST_FILTER_INVALID", fmt.Sprintf("unknown state filter %q", s), map[string]any{"name": "state", "value": s})
				return false, 0, 0, nil, false
			}
			want[s] = true
		}
	}
	return paged, page, size, want, true
}

func window(total, page, size int) (int, int) {
	start := min((page-1)*size, total)
	return start, min(start+size, total)
}

func (f *fakeAPI) nodeRow(n fakeNode, withToken bool) map[string]any {
	gpus := []any{}
	for i, g := range n.GPUs {
		gpus = append(gpus, map[string]any{"index": i, "name": g, "memory_total_mib": 81559})
	}
	row := map[string]any{
		"uuid": n.UUID, "name": n.Name, "hostname": n.Hostname, "state": n.State, "scheduling": "schedulable",
		"gpu_count": len(n.GPUs), "gpus": gpus, "labels": map[string]string{}, "has_ssh_credential": true,
		"last_seen_at": n.LastSeen.Format(time.RFC3339Nano), "created_at": n.Created.Format(time.RFC3339Nano),
	}
	if withToken {
		row["token"] = n.Token
	}
	return row
}

func (f *fakeAPI) listNodes(w http.ResponseWriter, req *http.Request) {
	paged, page, size, states, ok := listPage(w, req, "connecting", "online", "offline")
	if !ok {
		return
	}
	search := strings.ToLower(req.URL.Query().Get("q"))
	rows := []any{}
	for _, n := range f.nodes {
		if states != nil && !states[n.State] {
			continue
		}
		if search != "" && !strings.Contains(n.Name, search) && !strings.Contains(n.Hostname, search) && !strings.Contains(n.UUID, search) {
			continue
		}
		rows = append(rows, f.nodeRow(n, false))
	}
	if !paged {
		writeFake(w, 200, map[string]any{"items": rows, "total": len(rows)})
		return
	}
	start, end := window(len(rows), page, size)
	writeFake(w, 200, map[string]any{"items": rows[start:end], "page": page, "page_size": size, "total": len(rows), "all": len(f.nodes), "labels": []any{}})
}

func (f *fakeAPI) nodeDetail(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "uuid")
	for _, n := range f.nodes {
		if n.UUID == id {
			writeFake(w, 200, f.nodeRow(n, true))
			return
		}
	}
	writeProductErr(w, 404, "NODE_NOT_FOUND", "node not found", map[string]any{"node": id})
}

func (f *fakeAPI) listDeployments(w http.ResponseWriter, req *http.Request) {
	paged, page, size, states, ok := listPage(w, req, "running", "starting", "pulling", "pending", "failed", "stopped", "deleting")
	if !ok {
		return
	}
	rows := []any{}
	for _, d := range f.deps {
		if states == nil || states[d["runtime_state"].(string)] {
			rows = append(rows, d)
		}
	}
	if !paged {
		writeFake(w, 200, map[string]any{"items": rows, "total": len(rows)})
		return
	}
	start, end := window(len(rows), page, size)
	writeFake(w, 200, map[string]any{"items": rows[start:end], "page": page, "page_size": size, "total": len(rows)})
}

func (f *fakeAPI) searchLogs(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	if lv := q.Get("level"); lv != "" && lv != "error" && lv != "warn" && lv != "info" && lv != "debug" {
		writeProductErr(w, 400, "LOG_LEVEL_INVALID", fmt.Sprintf("unknown log level %q", lv), map[string]any{"level": lv})
		return
	}
	lines := []map[string]any{
		{"id": 9001, "instance": "inst-1", "node": uuidA, "level": "error", "source": "container", "time_ms": fakeNow.Add(-5 * time.Minute).UnixMilli(), "line": "NCCL WARN socket timeout"},
		{"id": 9000, "instance": "inst-1", "node": uuidA, "level": "info", "source": "container", "time_ms": fakeNow.Add(-6 * time.Minute).UnixMilli(), "line": "loading weights"},
	}
	items := []any{}
	for _, l := range lines {
		if node := q.Get("node"); node != "" && l["node"] != node {
			continue
		}
		if lv := q.Get("level"); lv != "" && l["level"] != lv {
			continue
		}
		if s := q.Get("q"); s != "" && !strings.Contains(l["line"].(string), s) {
			continue
		}
		items = append(items, l)
	}
	body := map[string]any{"items": items}
	if q.Get("with_total") == "1" {
		body["total"] = len(items)
	}
	writeFake(w, 200, body)
}

func fakeMetricCatalog() map[string]any {
	items := []any{
		map[string]any{"series": "gpuworks_gpu_memory_used_bytes", "name": "gpuworks_gpu_memory_used_bytes", "source": "gpu", "kind": "gauge", "unit": "bytes", "quantiles": false,
			"labels": []string{"node", "gpu", "gpu_uuid"}, "description": "GPU memory in use"},
		map[string]any{"series": "gpuworks_gpu_temperature_celsius", "name": "gpuworks_gpu_temperature_celsius", "source": "gpu", "kind": "gauge", "unit": "celsius", "quantiles": false,
			"labels": []string{"node", "gpu"}, "description": "GPU die temperature"},
		map[string]any{"series": "vllm:time_to_first_token_seconds", "name": "time_to_first_token_seconds", "source": "engine", "kind": "histogram", "unit": "seconds", "quantiles": true,
			"labels": []string{}, "description": "Time to first token"},
		map[string]any{"series": "gpuworks_node_rdma_rx_bytes", "name": "gpuworks_node_rdma_rx_bytes", "source": "network", "kind": "counter", "unit": "bytes", "quantiles": false,
			"labels": []string{"node", "hca"}, "description": "RDMA bytes received"},
	}
	return map[string]any{"items": items, "total": len(items)}
}

// apiEnv is a ToolEnv whose only source is an APIReader.
type apiEnv struct{ api APIReader }

func (e apiEnv) API() APIReader { return e.api }
func (e apiEnv) CompareWithHealthyReplica(string) (any, error) {
	return nil, errors.New("not in this fixture")
}
func (e apiEnv) GPUTopology(string) (any, error) { return nil, errors.New("not in this fixture") }

// callTool runs one built-in tool by name, as the runner does.
func callBuiltin(name string, env ToolEnv, args string) (string, error) {
	for _, t := range BuiltinTools() {
		if t.Name == name {
			return t.Call(context.Background(), env, json.RawMessage(args))
		}
	}
	return "", fmt.Errorf("no tool %s", name)
}

// atFakeNow pins the clock times are aged against for one test.
func atFakeNow(t interface{ Cleanup(func()) }) {
	saved := now
	now = func() time.Time { return fakeNow }
	t.Cleanup(func() { now = saved })
}
