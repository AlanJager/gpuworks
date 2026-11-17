package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// firstTunnelFailsHub refuses the first tunnel the gateway opens and dials
// the engine for every later one, whichever node is asked for.
type firstTunnelFailsHub struct {
	addr   string
	mu     sync.Mutex
	opened []string
}

func (h *firstTunnelFailsHub) OpenTunnel(_ context.Context, nodeUUID, _ string) (net.Conn, error) {
	h.mu.Lock()
	h.opened = append(h.opened, nodeUUID)
	first := len(h.opened) == 1
	h.mu.Unlock()
	if first {
		return nil, errors.New("tunnel refused")
	}
	return net.Dial("tcp", h.addr)
}

func (h *firstTunnelFailsHub) Online(string) bool { return true }

// Two flagless host replicas answer under their own nodes' model paths. When
// the replica a request was routed to cannot be reached, the gateway retries
// it on the other replica under that replica's name, and the client still
// sees the public name in the reply.
func TestGatewayRetriesBetweenHostReplicasThatAnswerUnderDifferentNames(t *testing.T) {
	const otherPath = "/Users/b/models/q"
	engine := &hostEngine{names: []string{hostModelPath, otherPath}}
	upstream := httptest.NewServer(engine)
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_host_engine_retry?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/q", Revision: "main", Name: "q", Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	for _, name := range []string{"mac-a", "mac-b"} {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		setRuntimeDriver(t, st, node.ID, proto.RuntimeDriverHost)
		nodes = append(nodes, node)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: nodes[0].ID, ServedName: "qwen", Engine: "vllm", State: "active", Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i, desired := range []string{flaglessHostDesired, strings.Replace(flaglessHostDesired, hostModelPath, otherPath, 1)} {
		if _, err := st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("mac-%d", i), DeploymentID: dep.ID, NodeID: nodes[i].ID, State: proto.InstRunning, APIPort: 30000, Ports: "[30000]", DesiredStateJSON: desired}); err != nil {
			t.Fatal(err)
		}
	}
	key, err := st.CreateAPIKey("host-engine-retry", 0)
	if err != nil {
		t.Fatal(err)
	}
	hub := &firstTunnelFailsHub{addr: strings.TrimPrefix(upstream.URL, "http://")}
	p := New(st, storeNetworks(st), hub, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)

	for _, stream := range []bool{false, true} {
		hub.mu.Lock()
		hub.opened = nil
		hub.mu.Unlock()
		engine.mu.Lock()
		engine.models = nil
		engine.mu.Unlock()
		body := `{"model":"qwen","stream":false,"messages":[{"role":"user","content":"hello"}]}`
		if stream {
			body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		p.Handler()(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%t: status %d: %s (tunnels %v, engine asked for %q)", stream, rec.Code, rec.Body.String(), hub.opened, engine.models)
		}
		if len(hub.opened) != 2 || hub.opened[0] == hub.opened[1] {
			t.Fatalf("stream=%t: tunnels opened %v, want the refused replica then the other", stream, hub.opened)
		}
		want := map[string]string{nodes[0].UUID: hostModelPath, nodes[1].UUID: otherPath}[hub.opened[1]]
		if len(engine.models) != 1 || engine.models[0] != want {
			t.Fatalf("stream=%t: engine asked for %q, want the retried replica's own name %q", stream, engine.models, want)
		}
		assertPublicReplyModel(t, rec.Body.String(), "qwen", stream)
	}
}
